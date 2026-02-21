package renderer

import (
	"fmt"

	"github.com/christhomas/docker-config-gen/internal/config"
)

// Renderer processes a template string with container data and returns rendered output.
type Renderer func(template string, containers []config.Container) (string, error)

// registry maps renderer names to their implementations.
var registry = map[string]Renderer{
	"nginx": Nginx,
}

// Get returns the renderer registered under the given name.
func Get(name string) (Renderer, error) {
	r, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("no renderer registered for %q", name)
	}
	return r, nil
}
