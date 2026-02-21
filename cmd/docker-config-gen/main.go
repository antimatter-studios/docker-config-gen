package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/christhomas/docker-config-gen/internal/docker"
	"github.com/christhomas/docker-config-gen/internal/management"
	"github.com/christhomas/docker-config-gen/internal/renderer"
)

func main() {
	log.Println("Starting docker config gen")

	dockerSocket := os.Getenv("DOCKER_SOCKET")
	if dockerSocket == "" {
		dockerSocket = "/var/run/docker.sock"
	}

	managementSocket := os.Getenv("MANAGEMENT_SOCKET")
	if managementSocket == "" {
		managementSocket = "/var/run/proxy/management.sock"
	}

	rendererName := os.Getenv("RENDERER")
	if rendererName == "" {
		rendererName = "nginx"
	}

	proxyContainer := os.Getenv("PROXY_CONTAINER")
	if proxyContainer == "" {
		proxyContainer = "docker-proxy"
	}

	debug := strings.ToLower(os.Getenv("DEBUG")) == "true"

	dockerClient, err := docker.NewClient(dockerSocket, debug)
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	mgmtClient := management.NewClient(managementSocket)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Start watching Docker events — returns channels, never blocks.
	eventCh, eventErrCh := dockerClient.WatchEvents(ctx)

	// Error rate limiter: suppress identical errors within a 10-second window.
	var (
		lastErrMsg  string
		lastErrTime time.Time
		errWindow   = 10 * time.Second
	)

	logUpdateError := func(prefix string, err error) {
		msg := err.Error()
		now := time.Now()
		if msg == lastErrMsg && now.Sub(lastErrTime) < errWindow {
			return // suppress duplicate within window
		}
		lastErrMsg = msg
		lastErrTime = now
		log.Printf("%s: %v", prefix, err)
	}

	// Initial configuration update on startup (non-blocking — logs errors if proxy isn't ready yet).
	if err := update(ctx, dockerClient, mgmtClient, proxyContainer, rendererName, debug); err != nil {
		logUpdateError("Initial update", err)
	}

	// Consume events with debouncing.
	// Rapid events (e.g. container start triggers network connect) are collapsed
	// into a single update after a quiet period.
	const debounce = 2 * time.Second
	var debounceTimer *time.Timer

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			log.Println("Shutting down docker config gen")
			return

		case err := <-eventErrCh:
			if err != nil && ctx.Err() == nil {
				log.Fatalf("Event watching failed: %v", err)
			}
			log.Println("Shutting down docker config gen")
			return

		case _, ok := <-eventCh:
			if !ok {
				// Channel closed, watcher stopped.
				log.Println("Event channel closed, shutting down")
				return
			}
			// Reset the debounce timer on each event.
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounce, func() {
				if err := update(ctx, dockerClient, mgmtClient, proxyContainer, rendererName, debug); err != nil {
					logUpdateError("Update error", err)
				} else {
					// Clear rate limiter on success so next error is always logged.
					lastErrMsg = ""
				}
			})
		}
	}
}

// lastConfig tracks the previously sent configuration to avoid unnecessary reloads.
var lastConfig string

// update discovers containers, syncs proxy networks, and generates nginx configuration.
// Each phase is independent — if the proxy isn't running, discovery and logging still work.
func update(ctx context.Context, dockerClient *docker.Client, mgmtClient *management.Client, proxyContainer string, rendererName string, debug bool) error {
	// Phase 1: Discover which networks have proxied containers.
	// This works regardless of the proxy's state.
	neededNetworks, err := dockerClient.DiscoverProxiedNetworks(ctx)
	if err != nil {
		return fmt.Errorf("discovering proxied networks: %w", err)
	}

	if len(neededNetworks) > 0 {
		names := make([]string, 0, len(neededNetworks))
		for name := range neededNetworks {
			names = append(names, name)
		}
		log.Printf("Proxied networks needed: %s", strings.Join(names, ", "))
	} else {
		log.Println("No proxied containers found")
	}

	// Phase 2: Sync proxy's network memberships.
	// Skip gracefully if the proxy container isn't running.
	if err := syncProxyNetworks(ctx, dockerClient, proxyContainer, neededNetworks); err != nil {
		log.Printf("Proxy network sync skipped: %v", err)
	}

	// Phase 3: Build container list using the proxy's (now-current) networks.
	// If the proxy isn't running, we can't build the config.
	networks, err := dockerClient.GetProxyNetworks(ctx, proxyContainer)
	if err != nil {
		return fmt.Errorf("proxy container not available: %w", err)
	}

	containerIDs, err := dockerClient.MakeContainerIDList(ctx, networks)
	if err != nil {
		return fmt.Errorf("listing containers on proxy networks: %w", err)
	}
	log.Printf("Found %d containers across %d proxy networks", len(containerIDs), len(networks))

	containerList, err := dockerClient.MakeContainerList(ctx, containerIDs, networks)
	if err != nil {
		return fmt.Errorf("building container list: %w", err)
	}

	// Phase 4: Render and send configuration.
	templateRenderer, err := renderer.Get(rendererName)
	if err != nil {
		return fmt.Errorf("no renderer for %q: %w", rendererName, err)
	}

	tmpl, err := mgmtClient.GetTemplate(ctx)
	if err != nil {
		return fmt.Errorf("fetching template from management server: %w", err)
	}

	output, err := templateRenderer(tmpl, containerList)
	if err != nil {
		return fmt.Errorf("rendering template: %w", err)
	}

	if debug && output != "" {
		log.Println("Rendered config:")
		log.Println(quoteString(output, ">    "))
	}

	// Skip sending if configuration hasn't changed.
	if output == lastConfig {
		log.Println("Configuration unchanged, skipping reload")
		return nil
	}

	log.Println("Sending configuration to management server...")
	if err := mgmtClient.SendConfig(ctx, output); err != nil {
		return fmt.Errorf("sending config to management server: %w", err)
	}

	lastConfig = output
	log.Println("Configuration updated successfully")
	return nil
}

// syncProxyNetworks connects the proxy to networks that have proxied containers
// and disconnects from networks that no longer need it.
func syncProxyNetworks(ctx context.Context, dockerClient *docker.Client, proxyContainer string, neededNetworks map[string]struct{}) error {
	proxyID, err := dockerClient.GetContainerID(ctx, proxyContainer)
	if err != nil {
		return fmt.Errorf("proxy container '%s' not found", proxyContainer)
	}

	currentNetworks, err := dockerClient.GetProxyNetworks(ctx, proxyContainer)
	if err != nil {
		return fmt.Errorf("getting proxy current networks: %w", err)
	}

	// Build lookup of current network names.
	currentNames := make(map[string]struct{})
	for _, net := range currentNetworks {
		currentNames[net.Name] = struct{}{}
	}

	// Connect to needed networks the proxy isn't on yet.
	for name := range neededNetworks {
		if _, ok := currentNames[name]; !ok {
			log.Printf("Connecting proxy to network '%s'", name)
			if err := dockerClient.ConnectNetwork(ctx, name, proxyID); err != nil {
				log.Printf("WARNING: could not connect proxy to network '%s': %v", name, err)
			}
		}
	}

	// Disconnect from networks that no longer have proxied containers.
	for _, net := range currentNetworks {
		if _, needed := neededNetworks[net.Name]; !needed {
			log.Printf("Disconnecting proxy from network '%s'", net.Name)
			if err := dockerClient.DisconnectNetwork(ctx, net.Name, proxyID); err != nil {
				log.Printf("WARNING: could not disconnect proxy from network '%s': %v", net.Name, err)
			}
		}
	}

	return nil
}

// quoteString prepends each line with a prefix (for debug log formatting).
func quoteString(input, prefix string) string {
	lines := strings.Split(input, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
