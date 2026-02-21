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

	"github.com/christhomas/docker-config-gen/internal/config"
	"github.com/christhomas/docker-config-gen/internal/docker"
	"github.com/christhomas/docker-config-gen/internal/renderer"
	"github.com/docker/docker/api/types/events"
)

const (
	maxRetries = 5
	retryDelay = 1 * time.Second
)

func main() {
	log.Println("Starting docker config gen")

	socketPath := os.Getenv("DOCKER_SOCKET")
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}

	debug := strings.ToLower(os.Getenv("DEBUG")) == "true"

	client, err := docker.NewClient(socketPath, debug)
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
	defer client.Close()

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

	// Initial configuration update on startup
	if err := update(ctx, client, debug); err != nil {
		log.Printf("Initial update error: %v", err)
	}

	// Watch for Docker network events
	err = client.WatchNetworkEvents(ctx, func(ctx context.Context, event events.Message) {
		if err := update(ctx, client, debug); err != nil {
			log.Printf("Update error: %v", err)
		}
	})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("Event watching failed: %v", err)
	}

	log.Println("Shutting down docker config gen")
}

// update generates configurations for all config-gen containers.
func update(ctx context.Context, client *docker.Client, debug bool) error {
	log.Println("Updating...")

	configList, err := client.MakeConfigList(ctx)
	if err != nil {
		return fmt.Errorf("making config list: %w", err)
	}

	for _, cfg := range configList {
		if err := processConfig(ctx, client, cfg, debug); err != nil {
			log.Printf("Error processing config %s: %v", cfg.Name, err)
		}
	}

	return nil
}

// processConfig handles template retrieval, rendering, and delivery for a single config.
func processConfig(ctx context.Context, client *docker.Client, cfg config.ConfigGen, debug bool) error {
	// Get unique container IDs from all relevant networks
	containerIDs, err := client.MakeContainerIDList(ctx, cfg.Networks)
	if err != nil {
		return fmt.Errorf("making container ID list: %w", err)
	}

	// Get full container metadata
	containerList, err := client.MakeContainerList(ctx, containerIDs, cfg.Networks)
	if err != nil {
		return fmt.Errorf("making container list: %w", err)
	}

	// Look up the renderer
	templateRenderer, err := renderer.Get(cfg.Renderer)
	if err != nil {
		log.Printf("There is no renderer enabled for this configuration: %s", cfg.Renderer)
		return nil
	}

	// Retry loop
	for retry := 0; retry < maxRetries; retry++ {
		// Request the template from the config container
		tmpl, err := client.ContainerExec(ctx, cfg.ID, cfg.Request, nil, true)
		if err != nil {
			logRetry(retry, err, debug)
			time.Sleep(retryDelay)
			continue
		}

		// Render the template with container data
		output, err := templateRenderer(tmpl, containerList)
		if err != nil {
			logRetry(retry, err, debug)
			time.Sleep(retryDelay)
			continue
		}

		// Send the rendered config back to the container
		response, err := client.ContainerExec(ctx, cfg.ID, cfg.Response, []string{output}, false)
		if err != nil {
			logRetry(retry, err, debug)
			time.Sleep(retryDelay)
			continue
		}

		if debug {
			log.Println("Response from container:")
			log.Println(quoteString(response, ">    "))
		}

		return nil
	}

	log.Printf("We have retried the maximum number of times, skipping over container %s!", cfg.Name)
	return nil
}

func logRetry(retry int, err error, debug bool) {
	if debug {
		log.Printf("Error information from container exec attempt: %v", err)
	}
	if retry+1 < maxRetries {
		log.Printf("Retrying '%d' of '%d'...", retry+1, maxRetries)
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
