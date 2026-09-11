package renderer

import (
	"fmt"

	"github.com/christhomas/docker-config-gen/internal/config"
)

// CertificateSource supplies the TLS certificate a host is served over HTTPS with.
type CertificateSource interface {
	// Certificate returns the certificate and key files for host, issuing or renewing
	// them as needed. changed reports that the files were just written; ok is false when
	// host can have no certificate, and it then stays HTTP-only.
	Certificate(host string) (certFile, keyFile string, changed, ok bool)
}

// Renderer processes a template string with container data and returns rendered output
// along with metadata about the generated configuration. certs may be nil, and every
// host is then rendered HTTP-only.
type Renderer func(template string, containers []config.Container, certs CertificateSource) (config.RenderResult, error)

// registry maps renderer names to their implementations.
var registry = map[string]Renderer{
	"nginx": NginxWithCertificates,
}

// Get returns the renderer registered under the given name.
func Get(name string) (Renderer, error) {
	r, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("no renderer registered for %q", name)
	}
	return r, nil
}
