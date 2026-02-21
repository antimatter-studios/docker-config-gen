package docker

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/christhomas/docker-config-gen/internal/config"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Client wraps the Docker SDK client with config-gen specific methods.
type Client struct {
	cli   *client.Client
	debug bool
}

// NewClient creates a new Docker client connected to the given socket.
func NewClient(socketPath string, debug bool) (*Client, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+socketPath),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}

	return &Client{cli: cli, debug: debug}, nil
}

// Close releases the Docker client resources.
func (c *Client) Close() error {
	return c.cli.Close()
}

// Inner returns the underlying Docker SDK client for use by other packages.
func (c *Client) Inner() *client.Client {
	return c.cli
}

// Debug returns whether debug mode is enabled.
func (c *Client) Debug() bool {
	return c.debug
}

// MakeConfigList filters running containers for those with docker-config-gen labels.
func (c *Client) MakeConfigList(ctx context.Context) ([]config.ConfigGen, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var configList []config.ConfigGen

	for _, ctr := range containers {
		labels, ok := getConfigGenLabels(ctr.Labels)
		if !ok {
			continue
		}

		name := ctr.Names[0]
		name = strings.TrimLeft(name, "/")

		networks := makeNetworkListFromSummary(ctr.NetworkSettings, nil, c.debug)

		configList = append(configList, config.ConfigGen{
			ID:       ctr.ID,
			Name:     name,
			Request:  labels.Request,
			Response: labels.Response,
			Renderer: labels.Renderer,
			Networks: networks,
		})
	}

	log.Println("Found configurations:")
	if len(configList) > 0 {
		for _, cfg := range configList {
			log.Printf("  - %s (renderer: %s, networks: %d)", cfg.Name, cfg.Renderer, len(cfg.Networks))
		}
	} else {
		log.Println("  Found no configurations...")
	}

	return configList, nil
}

// MakeContainerIDList returns a unique set of container IDs across all networks.
func (c *Client) MakeContainerIDList(ctx context.Context, networks config.NetworkMap) (map[string]struct{}, error) {
	idSet := make(map[string]struct{})

	for _, net := range networks {
		inspectData, err := c.cli.NetworkInspect(ctx, net.ID, network.InspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspecting network %s: %w", net.ID, err)
		}

		for id := range inspectData.Containers {
			idSet[id] = struct{}{}
		}
	}

	return idSet, nil
}

// MakeContainerList inspects each container and extracts its metadata.
func (c *Client) MakeContainerList(ctx context.Context, containerIDs map[string]struct{}, allowedNetworks config.NetworkMap) ([]config.Container, error) {
	var containers []config.Container

	for id := range containerIDs {
		inspectData, err := c.cli.ContainerInspect(ctx, id)
		if err != nil {
			log.Printf("WARNING: could not inspect container %s: %v", id, err)
			continue
		}

		name := strings.TrimLeft(inspectData.Name, "/")

		containers = append(containers, config.Container{
			ID:       inspectData.ID,
			Name:     name,
			Env:      makeEnvList(inspectData.Config.Env),
			Labels:   inspectData.Config.Labels,
			Networks: makeNetworkListFromInspect(inspectData.NetworkSettings.Networks, allowedNetworks, c.debug),
			Ports:    makePortList(inspectData.NetworkSettings.Ports),
		})
	}

	return containers, nil
}

// getConfigGenLabels extracts docker-config-gen labels from a container.
func getConfigGenLabels(labels map[string]string) (config.ConfigGenLabels, bool) {
	request, hasReq := labels["docker-config-gen.request"]
	response, hasResp := labels["docker-config-gen.response"]
	renderer, hasRend := labels["docker-config-gen.renderer"]

	if !hasReq || !hasResp || !hasRend {
		return config.ConfigGenLabels{}, false
	}

	return config.ConfigGenLabels{
		Request:  request,
		Response: response,
		Renderer: renderer,
	}, true
}

// makeNetworkListFromSummary converts container summary network settings to our NetworkMap.
func makeNetworkListFromSummary(settings *container.NetworkSettingsSummary, allowedNetworks config.NetworkMap, debug bool) config.NetworkMap {
	if settings == nil {
		return config.NetworkMap{}
	}

	result := config.NetworkMap{}
	for name, endpoint := range settings.Networks {
		if name == "bridge" {
			if debug {
				log.Printf("Skipping over network '%s' because we do not process bridge networks", name)
			}
			continue
		}

		if allowedNetworks != nil {
			if _, ok := allowedNetworks[endpoint.NetworkID]; !ok {
				if debug {
					log.Printf("Skipping over network '%s' because not in the allowed networks", name)
				}
				continue
			}
		}

		result[endpoint.NetworkID] = config.Network{
			Name:      name,
			ID:        endpoint.NetworkID,
			IPAddress: endpoint.IPAddress,
		}
	}
	return result
}

// makeNetworkListFromInspect converts full container inspect network settings to our NetworkMap.
func makeNetworkListFromInspect(networks map[string]*network.EndpointSettings, allowedNetworks config.NetworkMap, debug bool) config.NetworkMap {
	result := config.NetworkMap{}

	for name, endpoint := range networks {
		if endpoint == nil {
			continue
		}

		if name == "bridge" {
			if debug {
				log.Printf("Skipping over network '%s' because we do not process bridge networks", name)
			}
			continue
		}

		if allowedNetworks != nil {
			if _, ok := allowedNetworks[endpoint.NetworkID]; !ok {
				if debug {
					log.Printf("Skipping over network '%s' because not in the allowed networks", name)
				}
				continue
			}
		}

		result[endpoint.NetworkID] = config.Network{
			Name:      name,
			ID:        endpoint.NetworkID,
			IPAddress: endpoint.IPAddress,
		}
	}

	return result
}

// makeEnvList converts Docker's KEY=VALUE env strings to a map.
func makeEnvList(envVars []string) map[string]string {
	result := make(map[string]string)
	for _, entry := range envVars {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		} else if len(parts) == 1 {
			result[parts[0]] = ""
		}
	}
	return result
}

// makePortList converts Docker's nat.PortMap to our simplified Port slice.
func makePortList(ports nat.PortMap) []config.Port {
	var result []config.Port

	for portProto, bindings := range ports {
		parts := strings.SplitN(string(portProto), "/", 2)
		if len(parts) != 2 {
			log.Printf("ERROR: could not parse port/proto from %q", portProto)
			continue
		}
		containerPort := parts[0]
		containerProto := parts[1]

		if bindings == nil {
			result = append(result, config.Port{
				ContainerPort:  containerPort,
				ContainerProto: containerProto,
			})
			continue
		}

		for _, binding := range bindings {
			if binding.HostIP == "" || binding.HostPort == "" {
				log.Printf("ERROR: port binding for %s missing HostIP or HostPort: %+v", portProto, binding)
				continue
			}

			result = append(result, config.Port{
				ContainerPort:  containerPort,
				ContainerProto: containerProto,
				HostIP:         binding.HostIP,
				HostPort:       binding.HostPort,
			})
		}
	}

	return result
}
