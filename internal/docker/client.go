package docker

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/christhomas/docker-config-gen/internal/config"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
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

// GetProxyNetworks inspects the proxy container and returns its non-bridge networks.
func (c *Client) GetProxyNetworks(ctx context.Context, proxyContainer string) (config.NetworkMap, error) {
	inspectData, err := c.cli.ContainerInspect(ctx, proxyContainer)
	if err != nil {
		return nil, fmt.Errorf("inspecting proxy container %s: %w", proxyContainer, err)
	}

	networks := makeNetworkListFromInspect(inspectData.NetworkSettings.Networks, nil, c.debug)

	log.Printf("Proxy container '%s' is on %d network(s)", proxyContainer, len(networks))
	for _, net := range networks {
		log.Printf("  - %s (%s)", net.Name, net.ID[:12])
	}

	return networks, nil
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

// ListRunningContainerIDs returns the IDs of all running containers.
func (c *Client) ListRunningContainerIDs(ctx context.Context) ([]string, error) {
	f := filters.NewArgs()
	f.Add("status", "running")

	containers, err := c.cli.ContainerList(ctx, container.ListOptions{Filters: f})
	if err != nil {
		return nil, fmt.Errorf("listing running containers: %w", err)
	}

	ids := make([]string, len(containers))
	for i, ctr := range containers {
		ids[i] = ctr.ID
	}
	return ids, nil
}

// GetContainerID returns the container ID for a given name or ID.
func (c *Client) GetContainerID(ctx context.Context, nameOrID string) (string, error) {
	info, err := c.cli.ContainerInspect(ctx, nameOrID)
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", nameOrID, err)
	}
	return info.ID, nil
}

// ConnectNetwork connects a container to a Docker network.
func (c *Client) ConnectNetwork(ctx context.Context, networkName, containerID string) error {
	return c.cli.NetworkConnect(ctx, networkName, containerID, nil)
}

// DisconnectNetwork disconnects a container from a Docker network.
func (c *Client) DisconnectNetwork(ctx context.Context, networkName, containerID string) error {
	return c.cli.NetworkDisconnect(ctx, networkName, containerID, false)
}

// DiscoverProxiedNetworks inspects all running containers and returns the set
// of non-bridge network names that contain proxied containers (those with
// VIRTUAL_HOST env or docker-proxy.*.host labels). It picks the first
// non-bridge network per container.
func (c *Client) DiscoverProxiedNetworks(ctx context.Context) (map[string]struct{}, error) {
	ids, err := c.ListRunningContainerIDs(ctx)
	if err != nil {
		return nil, err
	}

	needed := make(map[string]struct{})

	for _, id := range ids {
		info, err := c.cli.ContainerInspect(ctx, id)
		if err != nil {
			continue
		}

		env := makeEnvList(info.Config.Env)
		if !isProxied(env, info.Config.Labels) {
			continue
		}

		containerName := strings.TrimLeft(info.Name, "/")

		// Pick the first non-bridge network.
		for name := range info.NetworkSettings.Networks {
			if name != "bridge" {
				needed[name] = struct{}{}
				if c.debug {
					log.Printf("Container '%s' needs proxy on network '%s'", containerName, name)
				}
				break
			}
		}
	}

	return needed, nil
}

// isProxied checks if a container has reverse proxy configuration
// (HTTP virtual host labels, TCP/UDP stream labels, or VIRTUAL_HOST env var).
func isProxied(env map[string]string, labels map[string]string) bool {
	if _, ok := env["VIRTUAL_HOST"]; ok {
		return true
	}
	for key, val := range labels {
		if !strings.HasPrefix(key, "docker-proxy.") {
			continue
		}
		// HTTP: docker-proxy.*.host
		if strings.HasSuffix(key, ".host") && len(val) > 0 {
			return true
		}
		// Stream: docker-proxy.*.proto = tcp or udp
		if strings.HasSuffix(key, ".proto") {
			proto := strings.ToLower(val)
			if proto == "tcp" || proto == "udp" {
				return true
			}
		}
	}
	return false
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
