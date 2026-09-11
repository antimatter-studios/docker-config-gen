package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/christhomas/docker-config-gen/internal/config"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Client wraps the Docker SDK client with config-gen specific methods.
type Client struct {
	cli   *client.Client
	debug bool
}

// NewClient creates a new Docker client connected to the given socket.
// The client negotiates the API version with the daemon on first use.
func NewClient(socketPath string, debug bool) (*Client, error) {
	cli, err := client.New(
		client.WithHost("unix://" + socketPath),
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
	res, err := c.cli.ContainerInspect(ctx, proxyContainer, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspecting proxy container %s: %w", proxyContainer, err)
	}
	inspectData := res.Container

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
		res, err := c.cli.NetworkInspect(ctx, net.ID, client.NetworkInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspecting network %s: %w", net.ID, err)
		}

		for id := range res.Network.Containers {
			idSet[id] = struct{}{}
		}
	}

	return idSet, nil
}

// MakeContainerList inspects each container and extracts its metadata.
func (c *Client) MakeContainerList(ctx context.Context, containerIDs map[string]struct{}, allowedNetworks config.NetworkMap) ([]config.Container, error) {
	var containers []config.Container

	for id := range containerIDs {
		res, err := c.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			log.Printf("WARNING: could not inspect container %s: %v", id, err)
			continue
		}
		inspectData := res.Container

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
	f := make(client.Filters)
	f.Add("status", "running")

	res, err := c.cli.ContainerList(ctx, client.ContainerListOptions{Filters: f})
	if err != nil {
		return nil, fmt.Errorf("listing running containers: %w", err)
	}

	ids := make([]string, len(res.Items))
	for i, ctr := range res.Items {
		ids[i] = ctr.ID
	}
	return ids, nil
}

// GetContainerID returns the container ID for a given name or ID.
func (c *Client) GetContainerID(ctx context.Context, nameOrID string) (string, error) {
	res, err := c.cli.ContainerInspect(ctx, nameOrID, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", nameOrID, err)
	}
	return res.Container.ID, nil
}

// ConnectNetwork connects a container to a Docker network.
func (c *Client) ConnectNetwork(ctx context.Context, networkName, containerID string) error {
	_, err := c.cli.NetworkConnect(ctx, networkName, client.NetworkConnectOptions{Container: containerID})
	return err
}

// DisconnectNetwork disconnects a container from a Docker network.
func (c *Client) DisconnectNetwork(ctx context.Context, networkName, containerID string) error {
	_, err := c.cli.NetworkDisconnect(ctx, networkName, client.NetworkDisconnectOptions{Container: containerID})
	return err
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
		res, err := c.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			continue
		}
		info := res.Container

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

		// An endpoint without an address decodes to the zero Addr, which
		// would print as "invalid IP"; keep it as an empty string instead.
		ipAddress := ""
		if endpoint.IPAddress.IsValid() {
			ipAddress = endpoint.IPAddress.String()
		}

		result[endpoint.NetworkID] = config.Network{
			Name:      name,
			ID:        endpoint.NetworkID,
			IPAddress: ipAddress,
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

// EnsureImage pulls an image if it is not already present locally.
func (c *Client) EnsureImage(ctx context.Context, ref string) error {
	_, err := c.cli.ImageInspect(ctx, ref)
	if err == nil {
		return nil // already present
	}

	log.Printf("Pulling image %s...", ref)
	reader, err := c.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", ref, err)
	}
	defer reader.Close()
	_, _ = io.Copy(io.Discard, reader)
	log.Printf("Image %s pulled successfully", ref)
	return nil
}

// CreateContainer creates a container with the given configuration and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, netCfg *network.NetworkingConfig, name string) (string, error) {
	resp, err := c.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           cfg,
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
		Name:             name,
	})
	if err != nil {
		return "", fmt.Errorf("creating container %s: %w", name, err)
	}
	return resp.ID, nil
}

// StartContainer starts a previously created container.
func (c *Client) StartContainer(ctx context.Context, containerID string) error {
	_, err := c.cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{})
	return err
}

// RemoveContainer force-removes a container (stops it if running).
func (c *Client) RemoveContainer(ctx context.Context, containerID string) error {
	_, err := c.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
	return err
}

// ListContainersByLabel returns containers matching all given labels (including stopped).
func (c *Client) ListContainersByLabel(ctx context.Context, labels map[string]string) ([]container.Summary, error) {
	f := make(client.Filters)
	for k, v := range labels {
		if v == "" {
			f.Add("label", k)
		} else {
			f.Add("label", k+"="+v)
		}
	}

	res, err := c.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: f,
	})
	return res.Items, err
}

// EnsureNetwork creates a Docker network if it doesn't already exist and returns its ID.
func (c *Client) EnsureNetwork(ctx context.Context, name string) (string, error) {
	// Check if network already exists.
	res, err := c.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		return res.Network.ID, nil
	}

	log.Printf("Creating network %s", name)
	resp, err := c.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		return "", fmt.Errorf("creating network %s: %w", name, err)
	}
	return resp.ID, nil
}

// makePortList converts Docker's network.PortMap to our simplified Port slice.
func makePortList(ports network.PortMap) []config.Port {
	var result []config.Port

	for portProto, bindings := range ports {
		if !portProto.IsValid() {
			log.Printf("ERROR: could not parse port/proto from %q", portProto)
			continue
		}
		containerPort := portProto.Port()
		containerProto := string(portProto.Proto())

		if bindings == nil {
			result = append(result, config.Port{
				ContainerPort:  containerPort,
				ContainerProto: containerProto,
			})
			continue
		}

		for _, binding := range bindings {
			if !binding.HostIP.IsValid() || binding.HostPort == "" {
				log.Printf("ERROR: port binding for %s missing HostIP or HostPort: %+v", portProto, binding)
				continue
			}

			result = append(result, config.Port{
				ContainerPort:  containerPort,
				ContainerProto: containerProto,
				HostIP:         binding.HostIP.String(),
				HostPort:       binding.HostPort,
			})
		}
	}

	return result
}
