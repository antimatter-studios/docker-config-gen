package docker

import (
	"context"
	"log"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
)

// WatchEvents listens for Docker container and network events and sends them
// to the returned channel. The caller is responsible for consuming events.
// The channel is closed when the context is cancelled or an error occurs.
// Any error is sent to the returned error channel before both channels close.
func (c *Client) WatchEvents(ctx context.Context) (<-chan events.Message, <-chan error) {
	out := make(chan events.Message, 16)
	errOut := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errOut)

		filter := filters.NewArgs()

		// Container lifecycle events.
		filter.Add("type", string(events.ContainerEventType))
		filter.Add("event", "start")
		filter.Add("event", "die")

		// Network membership events.
		filter.Add("type", string(events.NetworkEventType))
		filter.Add("event", "connect")
		filter.Add("event", "disconnect")

		eventCh, dockerErrCh := c.cli.Events(ctx, events.ListOptions{
			Filters: filter,
		})

		log.Println("Listening for Docker events...")

		for {
			select {
			case <-ctx.Done():
				return

			case err := <-dockerErrCh:
				if err != nil {
					errOut <- err
				}
				return

			case event := <-eventCh:
				// Skip bridge network events.
				if event.Type == events.NetworkEventType {
					if event.Actor.Attributes["name"] == "bridge" {
						continue
					}
				}

				logEvent(event)

				if c.debug {
					log.Printf("  Event details: %+v", event)
				}

				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, errOut
}

// logEvent prints a human-readable summary of a Docker event.
func logEvent(event events.Message) {
	attrs := event.Actor.Attributes

	switch event.Type {
	case events.ContainerEventType:
		name := attrs["name"]
		image := attrs["image"]
		if event.Action == "start" {
			log.Printf("Container started: %s (%s)", name, image)
		} else if event.Action == "die" {
			exitCode := attrs["exitCode"]
			log.Printf("Container stopped: %s (%s) exit=%s", name, image, exitCode)
		}

	case events.NetworkEventType:
		networkName := attrs["name"]
		containerID := attrs["container"]
		if len(containerID) > 12 {
			containerID = containerID[:12]
		}
		log.Printf("Network %s: container %s on %s", event.Action, containerID, networkName)
	}
}
