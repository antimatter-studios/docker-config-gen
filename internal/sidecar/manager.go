package sidecar

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/christhomas/docker-config-gen/internal/config"
	"github.com/christhomas/docker-config-gen/internal/docker"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
)

const (
	// SidecarImage is the socat image used for port-forwarding sidecars.
	SidecarImage = "alpine/socat"

	// SidecarNetwork is the dedicated network for sidecar-to-proxy communication.
	SidecarNetwork = "docker-proxy-sidecars"

	// LabelSidecarPort records which port the sidecar forwards.
	LabelSidecarPort = "docker-proxy.sidecar.port"

	// LabelSidecarProto records the protocol (tcp or udp).
	LabelSidecarProto = "docker-proxy.sidecar.proto"
)

// Manager handles creation and removal of sidecar containers that publish
// TCP/UDP ports to the host and forward traffic to the proxy container.
type Manager struct {
	docker         *docker.Client
	proxyContainer string
	debug          bool
}

// NewManager creates a sidecar manager.
func NewManager(dockerClient *docker.Client, proxyContainer string, debug bool) *Manager {
	return &Manager{
		docker:         dockerClient,
		proxyContainer: proxyContainer,
		debug:          debug,
	}
}

// sidecarSpec describes a sidecar that should exist.
type sidecarSpec struct {
	Name      string
	Port      int
	Protocol  string // "tcp" or "udp"
	ProxyHost string // container name to forward to
}

// ContainerName builds the predictable sidecar container name.
func ContainerName(proto string, port int) string {
	return fmt.Sprintf("docker-proxy-sidecar-%s-%d", proto, port)
}

// desiredSidecars converts stream ports into a map keyed by container name.
func desiredSidecars(streamPorts []config.StreamPort, proxyContainer string) map[string]sidecarSpec {
	desired := make(map[string]sidecarSpec)
	for _, sp := range streamPorts {
		name := ContainerName(sp.Protocol, sp.ListenPort)
		desired[name] = sidecarSpec{
			Name:      name,
			Port:      sp.ListenPort,
			Protocol:  sp.Protocol,
			ProxyHost: proxyContainer,
		}
	}
	return desired
}

// existingSidecars discovers all managed sidecar containers (running or stopped).
func (m *Manager) existingSidecars(ctx context.Context) (map[string]string, error) {
	containers, err := m.docker.ListContainersByLabel(ctx, map[string]string{
		config.SidecarLabel: "true",
	})
	if err != nil {
		return nil, fmt.Errorf("listing sidecar containers: %w", err)
	}

	// Map container name -> container ID
	result := make(map[string]string)
	for _, ctr := range containers {
		for _, name := range ctr.Names {
			cleanName := strings.TrimLeft(name, "/")
			result[cleanName] = ctr.ID
			break
		}
	}
	return result, nil
}

// Reconcile ensures sidecar containers match the desired stream ports.
// It creates missing sidecars and removes stale ones.
func (m *Manager) Reconcile(ctx context.Context, streamPorts []config.StreamPort) error {
	desired := desiredSidecars(streamPorts, m.proxyContainer)
	existing, err := m.existingSidecars(ctx)
	if err != nil {
		return err
	}

	// Nothing to do if both are empty.
	if len(desired) == 0 && len(existing) == 0 {
		return nil
	}

	// Ensure the dedicated sidecar network exists and proxy is connected.
	networkID, err := m.ensureSidecarNetwork(ctx)
	if err != nil {
		return fmt.Errorf("ensuring sidecar network: %w", err)
	}

	// Ensure socat image is available if we need to create any sidecars.
	needsCreate := false
	for name := range desired {
		if _, exists := existing[name]; !exists {
			needsCreate = true
			break
		}
	}
	if needsCreate {
		if err := m.docker.EnsureImage(ctx, SidecarImage); err != nil {
			return fmt.Errorf("ensuring sidecar image: %w", err)
		}
	}

	// Create missing sidecars.
	for name, spec := range desired {
		if _, exists := existing[name]; !exists {
			log.Printf("Creating sidecar %s (%s/%d)", name, spec.Protocol, spec.Port)
			if err := m.createSidecar(ctx, spec, networkID); err != nil {
				log.Printf("ERROR: failed to create sidecar %s: %v", name, err)
			}
		} else if m.debug {
			log.Printf("Sidecar %s already exists", name)
		}
	}

	// Remove stale sidecars.
	for name, id := range existing {
		if _, needed := desired[name]; !needed {
			log.Printf("Removing stale sidecar %s", name)
			if err := m.docker.RemoveContainer(ctx, id); err != nil {
				log.Printf("ERROR: failed to remove sidecar %s: %v", name, err)
			}
		}
	}

	return nil
}

// ensureSidecarNetwork creates the dedicated sidecar network and connects
// the proxy container to it.
func (m *Manager) ensureSidecarNetwork(ctx context.Context) (string, error) {
	networkID, err := m.docker.EnsureNetwork(ctx, SidecarNetwork)
	if err != nil {
		return "", err
	}

	// Connect proxy to the sidecar network (idempotent — Docker ignores if already connected).
	if err := m.docker.ConnectNetwork(ctx, SidecarNetwork, m.proxyContainer); err != nil {
		// Ignore "already connected" errors.
		if !strings.Contains(err.Error(), "already exists") {
			log.Printf("WARNING: could not connect proxy to sidecar network: %v", err)
		}
	}

	return networkID, nil
}

// createSidecar creates and starts a single sidecar container.
func (m *Manager) createSidecar(ctx context.Context, spec sidecarSpec, networkID string) error {
	portStr := strconv.Itoa(spec.Port)
	protoUpper := strings.ToUpper(spec.Protocol)

	// socat command: listen on port, fork for each connection, forward to proxy
	// alpine/socat entrypoint is "socat", so Cmd is just the two address arguments
	listenAddr := fmt.Sprintf("%s-LISTEN:%d,fork,reuseaddr", protoUpper, spec.Port)
	forwardAddr := fmt.Sprintf("%s:%s:%d", protoUpper, spec.ProxyHost, spec.Port)

	portNat, err := nat.NewPort(spec.Protocol, portStr)
	if err != nil {
		return fmt.Errorf("creating nat port: %w", err)
	}

	cfg := &container.Config{
		Image: SidecarImage,
		Cmd:   []string{listenAddr, forwardAddr},
		Labels: map[string]string{
			config.SidecarLabel: "true",
			LabelSidecarPort:    portStr,
			LabelSidecarProto:   spec.Protocol,
		},
		ExposedPorts: nat.PortSet{
			portNat: struct{}{},
		},
	}

	hostCfg := &container.HostConfig{
		PortBindings: nat.PortMap{
			portNat: []nat.PortBinding{
				{HostIP: "0.0.0.0", HostPort: portStr},
			},
		},
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyUnlessStopped,
		},
	}

	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			SidecarNetwork: {
				NetworkID: networkID,
			},
		},
	}

	id, err := m.docker.CreateContainer(ctx, cfg, hostCfg, netCfg, spec.Name)
	if err != nil {
		return err
	}

	if err := m.docker.StartContainer(ctx, id); err != nil {
		_ = m.docker.RemoveContainer(ctx, id)
		return fmt.Errorf("starting sidecar %s: %w", spec.Name, err)
	}

	log.Printf("Sidecar %s started (container %s)", spec.Name, id[:12])
	return nil
}

// RemoveAll stops and removes all managed sidecar containers.
func (m *Manager) RemoveAll(ctx context.Context) {
	existing, err := m.existingSidecars(ctx)
	if err != nil {
		log.Printf("WARNING: could not list sidecars during cleanup: %v", err)
		return
	}

	for name, id := range existing {
		log.Printf("Removing sidecar %s", name)
		if err := m.docker.RemoveContainer(ctx, id); err != nil {
			log.Printf("WARNING: failed to remove sidecar %s: %v", name, err)
		}
	}
}
