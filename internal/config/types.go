package config

// ConfigGenLabels are the docker labels that identify a config-gen container.
type ConfigGenLabels struct {
	Request  string
	Response string
	Renderer string
}

// ConfigGen represents a container that requests config generation.
type ConfigGen struct {
	ID       string
	Name     string
	Request  string
	Response string
	Renderer string
	Networks NetworkMap
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
}

// Server is an nginx server block definition.
type Server struct {
	Host      string
	Locations []Location
}

// VirtualHost captures the virtual hosting parameters for a container.
type VirtualHost struct {
	Host        string
	Port        int
	Path        string
	PathIsRegex bool
	Protocol    string
}
