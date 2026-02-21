package renderer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"text/template"

	"github.com/christhomas/docker-config-gen/internal/config"
)

// Nginx renders an Nginx configuration from a Go text/template and container metadata.
// The template receives .ErrorPageData, .ServerList, and .UpstreamList.
func Nginx(tmpl string, containerList []config.Container) (string, error) {
	log.Println("Processing template...")

	// Filter containers to only those with valid upstream configurations
	upstreams := filterValidUpstreams(containerList)

	// Filter env vars and labels to only relevant ones
	for i := range upstreams {
		upstreams[i].Env = filterEnvVars(upstreams[i].Env)
		upstreams[i].Labels = filterLabels(upstreams[i].Labels)
	}

	serverMap := make(map[string]*config.Server)
	upstreamMap := make(map[string]*config.Upstream)
	var errorPageData []string

	for _, ctr := range upstreams {
		virtualHostList := makeVirtualHostList(ctr)
		var processedPaths []string

		for _, vh := range virtualHostList {
			var networkLocations []config.NetworkLocation

			// Each container might be on multiple networks; add each as a fallback
			for _, net := range ctr.Networks {
				networkLocations = append(networkLocations, config.NetworkLocation{
					Name:      net.Name,
					IPAddress: net.IPAddress,
					Port:      vh.Port,
				})
			}

			// Create upstream: protocol_containername_port
			upstreamName := fmt.Sprintf("%s_%s_%d", vh.Protocol, ctr.Name, vh.Port)
			upstreamMap[upstreamName] = &config.Upstream{
				Name:     upstreamName,
				Networks: networkLocations,
			}

			// Create or get server block for this hostname
			if _, exists := serverMap[vh.Host]; !exists {
				serverMap[vh.Host] = &config.Server{
					Host:      vh.Host,
					Locations: nil,
				}
			}

			// Add location if not already processed
			if !containsString(processedPaths, vh.Path) {
				serverMap[vh.Host].Locations = append(serverMap[vh.Host].Locations, config.Location{
					Path:        vh.Path,
					PathIsRegex: vh.PathIsRegex,
					Protocol:    vh.Protocol,
					Upstream:    upstreamName,
				})
				processedPaths = append(processedPaths, vh.Path)
			}

			// Build error page data as base64-encoded JSON
			errorInfo := map[string]string{
				"protocol":  vh.Protocol,
				"host":      vh.Host,
				"path":      vh.Path,
				"container": ctr.Name,
			}
			jsonBytes, _ := json.Marshal(errorInfo)
			errorPageData = append(errorPageData, base64.StdEncoding.EncodeToString(jsonBytes))
		}
	}

	// Convert maps to slices
	var servers []config.Server
	for _, s := range serverMap {
		servers = append(servers, *s)
	}
	var upstreamList []config.Upstream
	for _, u := range upstreamMap {
		upstreamList = append(upstreamList, *u)
	}

	if len(servers) == 0 && len(upstreamList) == 0 {
		log.Println("There are no servers or upstreams found")
		return "", nil
	}

	data := map[string]any{
		"ErrorPageData": errorPageData,
		"ServerList":    servers,
		"UpstreamList":  upstreamList,
	}

	rendered, err := renderTemplate(tmpl, data)
	if err != nil {
		log.Printf("Template render error: %v", err)
		return "", err
	}

	return rendered, nil
}

func renderTemplate(tmpl string, data any) (string, error) {
	log.Println("Writing template...")

	t, err := template.New("nginx").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return reformatTemplate(buf.String()), nil
}

func makeVirtualHostFromEnv(env map[string]string) config.VirtualHost {
	port := 80
	if p, ok := env["VIRTUAL_PORT"]; ok {
		if v, err := strconv.Atoi(p); err == nil {
			port = v
		}
	}

	protocol := "http"
	if p, ok := env["VIRTUAL_PROTO"]; ok {
		protocol = p
	}

	path := "/"
	if p, ok := env["VIRTUAL_PATH"]; ok {
		path = strings.TrimLeft(p, "~")
	}

	return config.VirtualHost{
		Host:        env["VIRTUAL_HOST"],
		Port:        port,
		Path:        path,
		PathIsRegex: strings.HasPrefix(path, "^"),
		Protocol:    protocol,
	}
}

func makeVirtualHostFromLabels(dockerProxy string, group string, labels map[string]string) config.VirtualHost {
	port := 80
	if p, ok := labels[dockerProxy+"."+group+".port"]; ok {
		if v, err := strconv.Atoi(p); err == nil {
			port = v
		}
	}

	protocol := "http"
	if p, ok := labels[dockerProxy+"."+group+".protocol"]; ok {
		protocol = p
	}

	path := "/"
	if p, ok := labels[dockerProxy+"."+group+".path"]; ok {
		path = strings.TrimLeft(p, "~")
	}

	return config.VirtualHost{
		Host:        labels[dockerProxy+"."+group+".host"],
		Port:        port,
		Path:        path,
		PathIsRegex: strings.HasPrefix(path, "^"),
		Protocol:    protocol,
	}
}

func makeVirtualHostList(ctr config.Container) []config.VirtualHost {
	var list []config.VirtualHost

	// Check for VIRTUAL_HOST environment variable
	if _, ok := ctr.Env["VIRTUAL_HOST"]; ok {
		list = append(list, makeVirtualHostFromEnv(ctr.Env))
	}

	// Check for docker-proxy.* labels (grouped by middle segment)
	processed := make(map[string]bool)
	for key := range ctr.Labels {
		parts := strings.SplitN(key, ".", 3)
		if len(parts) != 3 {
			continue
		}
		group := parts[1]

		if processed[group] {
			continue
		}

		list = append(list, makeVirtualHostFromLabels(parts[0], group, ctr.Labels))
		processed[group] = true
	}

	return list
}

func filterEnvVars(env map[string]string) map[string]string {
	filtered := make(map[string]string)
	for key, val := range env {
		if strings.HasPrefix(key, "VIRTUAL") {
			filtered[key] = val
		}
	}
	return filtered
}

func filterLabels(labels map[string]string) map[string]string {
	filtered := make(map[string]string)
	for key, val := range labels {
		if strings.HasPrefix(key, "docker-proxy") {
			filtered[key] = val
		}
	}
	return filtered
}

func filterValidUpstreams(containers []config.Container) []config.Container {
	var valid []config.Container

	for _, ctr := range containers {
		// Check for docker-proxy.*.host labels
		hasHost := false
		for key, val := range ctr.Labels {
			parts := strings.SplitN(key, ".", 3)
			if len(parts) != 3 {
				continue
			}
			if parts[0] != "docker-proxy" {
				continue
			}
			if parts[2] == "host" && len(val) > 0 {
				hasHost = true
				break
			}
		}

		// Check for VIRTUAL_HOST env var
		if !hasHost {
			if _, ok := ctr.Env["VIRTUAL_HOST"]; ok {
				hasHost = true
			}
		}

		if hasHost {
			// Deep copy to avoid mutating the original
			clone := config.Container{
				ID:       ctr.ID,
				Name:     ctr.Name,
				Env:      copyMap(ctr.Env),
				Labels:   copyMap(ctr.Labels),
				Networks: ctr.Networks,
				Ports:    ctr.Ports,
			}
			valid = append(valid, clone)
		}
	}

	return valid
}

// reformatTemplate cleans up the rendered template with proper indentation.
// This mirrors the TypeScript reformatTemplate function.
func reformatTemplate(tmpl string) string {
	indent := "    "
	count := 0

	lines := strings.Split(tmpl, "\n")
	var doc []string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Add blank line before block openers (unless preceded by a comment)
		if strings.HasSuffix(line, "{") && len(doc) > 0 && !strings.HasPrefix(doc[len(doc)-1], "#") {
			doc = append(doc, "")
		}

		// Closing brace decreases indent for this line
		if strings.HasPrefix(line, "}") {
			count--
		}

		// Add non-empty lines with indentation
		if len(line) > 0 {
			count = int(math.Abs(float64(count)))
			doc = append(doc, strings.Repeat(indent, count)+line)
		}

		// Opening brace increases indent for next line
		if strings.HasSuffix(line, "{") {
			count++
		}

		// Add blank line after closing brace (unless next is also closing)
		if line == "}" && len(doc) > 0 && !strings.HasSuffix(doc[len(doc)-1], "}") {
			doc = append(doc, "")
		}
	}

	return strings.Join(doc, "\n") + "\n"
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func copyMap(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
