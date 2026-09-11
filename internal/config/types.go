package config

const (
	// SidecarLabel is the Docker label that identifies managed sidecar containers.
	SidecarLabel = "docker-proxy.sidecar"
)

// RenderResult holds both the rendered configuration text and the
// parsed stream port metadata extracted during rendering.
type RenderResult struct {
	Config      string
	StreamPorts []StreamPort
	// CertificatesChanged reports that a host's certificate files were written during
	// this render. nginx reads certificates only when it loads its configuration, so the
	// proxy must reload even when the configuration text is unchanged.
	CertificatesChanged bool
}

// Network holds network metadata for a container endpoint.
type Network struct {
	Name      string
	IPAddress string
	ID        string
}

// NetworkMap maps network IDs to Network info.
type NetworkMap map[string]Network

// Port holds a container port mapping.
type Port struct {
	ContainerPort  string
	ContainerProto string
	HostIP         string
	HostPort       string
}

// Container holds the extracted metadata for a running container.
type Container struct {
	ID       string
	Name     string
	Env      map[string]string
	Labels   map[string]string
	Networks NetworkMap
	Ports    []Port
}

// NetworkLocation is a target address for an upstream server.
type NetworkLocation struct {
	Name      string
	IPAddress string
	Port      int
}

// Upstream is an nginx upstream block definition.
type Upstream struct {
	Name     string
	Networks []NetworkLocation
}

// Location is a path-based routing entry within a server block.
type Location struct {
	Path        string
	PathIsRegex bool
	Protocol    string
	Upstream    string
	// MaxBodySize overrides the proxy-wide upload limit for this location, from
	// `docker-proxy.<group>.max_body_size`. Empty means inherit the default.
	MaxBodySize string
}

// Server is an nginx server block definition.
type Server struct {
	Host      string
	Locations []Location
	// Certificate and CertificateKey are the files the proxy serves HTTPS for Host
	// with. Both are empty when the host has no certificate, and it then stays HTTP-only.
	Certificate    string
	CertificateKey string
}

// VirtualHost captures the virtual hosting parameters for a container.
type VirtualHost struct {
	Host        string
	Port        int
	Path        string
	PathIsRegex bool
	Protocol    string
	MaxBodySize string
}

// StreamPort groups all upstreams sharing the same proxy listen port.
type StreamPort struct {
	ListenPort      int
	Protocol        string // "tcp" or "udp"
	HasSNI          bool
	DefaultUpstream string
	SNIEntries      []StreamSNIEntry
}

// StreamSNIEntry maps an SNI hostname to a stream upstream.
type StreamSNIEntry struct {
	Host     string
	Upstream string
}

// StreamUpstream is a named group of backend addresses for stream proxying.
type StreamUpstream struct {
	Name     string
	Networks []NetworkLocation
}
