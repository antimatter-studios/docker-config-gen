package docker

import (
	"context"
	"log"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
)

// EventHandler is called when a network event occurs.
type EventHandler func(ctx context.Context, event events.Message)

// WatchNetworkEvents listens for Docker network connect/disconnect events
// and calls the handler for each one (skipping bridge network events).
func (c *Client) WatchNetworkEvents(ctx context.Context, handler EventHandler) error {
	filter := filters.NewArgs()
	filter.Add("type", string(events.NetworkEventType))
	filter.Add("event", "connect")
	filter.Add("event", "disconnect")

	eventCh, errCh := c.cli.Events(ctx, events.ListOptions{
		Filters: filter,
	})

	log.Println("Listening for Docker network events...")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errCh:
			if err != nil {
				return err
			}
			return nil

		case event := <-eventCh:
			networkName := event.Actor.Attributes["name"]

			// Ignore events on the bridge network
			if networkName == "bridge" {
				continue
			}

			// Delay to allow containers to stabilize after network changes
			time.Sleep(1 * time.Second)

			if c.debug {
				log.Printf("Network event: %+v", event)
			} else {
				log.Printf("Network %s: %s", event.Action, networkName)
			}

			handler(ctx, event)
		}
	}
}
