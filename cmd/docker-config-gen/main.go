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
	"github.com/docker/docker/api/types/events"
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

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Wait for management server to be ready
	waitForManagement(ctx, mgmtClient)

	// Initial configuration update on startup
	if err := update(ctx, dockerClient, mgmtClient, proxyContainer, rendererName, debug); err != nil {
		log.Printf("Initial update error: %v", err)
	}

	// Watch for Docker network events
	err = dockerClient.WatchNetworkEvents(ctx, func(ctx context.Context, event events.Message) {
		if err := update(ctx, dockerClient, mgmtClient, proxyContainer, rendererName, debug); err != nil {
			log.Printf("Update error: %v", err)
		}
	})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("Event watching failed: %v", err)
	}

	log.Println("Shutting down docker config gen")
}

// update discovers containers and generates nginx configuration.
func update(ctx context.Context, dockerClient *docker.Client, mgmtClient *management.Client, proxyContainer string, rendererName string, debug bool) error {
	log.Println("Updating...")

	// Get the proxy container's networks
	networks, err := dockerClient.GetProxyNetworks(ctx, proxyContainer)
	if err != nil {
		return fmt.Errorf("getting proxy networks: %w", err)
	}

	// Get unique container IDs from all proxy networks
	containerIDs, err := dockerClient.MakeContainerIDList(ctx, networks)
	if err != nil {
		return fmt.Errorf("making container ID list: %w", err)
	}

	// Get full container metadata
	containerList, err := dockerClient.MakeContainerList(ctx, containerIDs, networks)
	if err != nil {
		return fmt.Errorf("making container list: %w", err)
	}

	// Look up the renderer
	templateRenderer, err := renderer.Get(rendererName)
	if err != nil {
		return fmt.Errorf("no renderer for %q: %w", rendererName, err)
	}

	// Request the template from the management server
	tmpl, err := mgmtClient.GetTemplate(ctx)
	if err != nil {
		return fmt.Errorf("getting template: %w", err)
	}

	// Render the template with container data
	output, err := templateRenderer(tmpl, containerList)
	if err != nil {
		return fmt.Errorf("rendering template: %w", err)
	}

	if debug && output != "" {
		log.Println("Rendered config:")
		log.Println(quoteString(output, ">    "))
	}

	// Send the rendered config to the proxy (empty string triggers reset)
	if err := mgmtClient.SendConfig(ctx, output); err != nil {
		return fmt.Errorf("sending config: %w", err)
	}

	return nil
}

// waitForManagement polls the management server until it's ready.
func waitForManagement(ctx context.Context, mgmt *management.Client) {
	log.Println("Waiting for management server...")
	for {
		select {
		case <-ctx.Done():
			log.Fatalf("Context cancelled while waiting for management server")
		default:
			if err := mgmt.Health(ctx); err == nil {
				log.Println("Management server is ready")
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// quoteString prepends each line with a prefix (for debug log formatting).
func quoteString(input, prefix string) string {
	lines := strings.Split(input, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
