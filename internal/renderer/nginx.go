package renderer

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"

	"github.com/christhomas/docker-config-gen/internal/config"
	"github.com/flosch/pongo2/v6"
)

// streamEntry holds parsed TCP/UDP label data for a single group (internal to renderer).
type streamEntry struct {
	ListenPort int
	Port       int
	Protocol   string // "tcp" or "udp"
	Host       string // optional, for SNI routing
}

// Nginx renders an Nginx configuration from a Pongo2 (Jinja2) template and container metadata.
// The template receives ErrorPageData, ServerList, UpstreamList, StreamPortList, and StreamUpstreamList.
func Nginx(tmpl string, containerList []config.Container) (config.RenderResult, error) {
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

	// Stream data: keyed by listen port
	streamUpstreamMap := make(map[string]*config.StreamUpstream)
	type portInfo struct {
		protocol string
		entries  []struct {
			host     string
			upstream string
		}
		defaultUpstream string
	}
	streamPortMap := make(map[int]*portInfo)

	for _, ctr := range upstreams {
		// Process HTTP virtual hosts
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

		// Process TCP/UDP stream entries
		streamEntries := makeStreamEntryList(ctr)
		for _, se := range streamEntries {
			var networkLocations []config.NetworkLocation
			for _, net := range ctr.Networks {
				networkLocations = append(networkLocations, config.NetworkLocation{
					Name:      net.Name,
					IPAddress: net.IPAddress,
					Port:      se.Port,
				})
			}

			upstreamName := fmt.Sprintf("%s_%s_%d", se.Protocol, ctr.Name, se.Port)
			streamUpstreamMap[upstreamName] = &config.StreamUpstream{
				Name:     upstreamName,
				Networks: networkLocations,
			}

			// Group by listen port
			pi, exists := streamPortMap[se.ListenPort]
			if !exists {
				pi = &portInfo{protocol: se.Protocol}
				streamPortMap[se.ListenPort] = pi
			}

			// Set default upstream (first one seen for this port)
			if pi.defaultUpstream == "" {
				pi.defaultUpstream = upstreamName
			}

			// If host is specified, add SNI entry
			if se.Host != "" {
				pi.entries = append(pi.entries, struct {
					host     string
					upstream string
				}{host: se.Host, upstream: upstreamName})
			}

			// Build error page data for stream entries too
			errorInfo := map[string]string{
				"protocol":  se.Protocol,
				"host":      se.Host,
				"path":      "",
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

	// Build stream port list
	var streamPortList []config.StreamPort
	for listenPort, pi := range streamPortMap {
		sp := config.StreamPort{
			ListenPort:      listenPort,
			Protocol:        pi.protocol,
			HasSNI:          len(pi.entries) > 0,
			DefaultUpstream: pi.defaultUpstream,
		}
		for _, e := range pi.entries {
			sp.SNIEntries = append(sp.SNIEntries, config.StreamSNIEntry{
				Host:     e.host,
				Upstream: e.upstream,
			})
		}
		streamPortList = append(streamPortList, sp)
	}

	var streamUpstreamList []config.StreamUpstream
	for _, u := range streamUpstreamMap {
		streamUpstreamList = append(streamUpstreamList, *u)
	}

	hasHTTP := len(servers) > 0 || len(upstreamList) > 0
	hasStream := len(streamPortList) > 0

	if !hasHTTP && !hasStream {
		log.Println("There are no servers or upstreams found")
		return config.RenderResult{}, nil
	}

	data := pongo2.Context{
		"ErrorPageData":      errorPageData,
		"ServerList":         servers,
		"UpstreamList":       upstreamList,
		"StreamPortList":     streamPortList,
		"StreamUpstreamList": streamUpstreamList,
	}

	rendered, err := renderTemplate(tmpl, data)
	if err != nil {
		log.Printf("Template render error: %v", err)
		return config.RenderResult{}, err
	}

	return config.RenderResult{
		Config:      rendered,
		StreamPorts: streamPortList,
	}, nil
}

func renderTemplate(tmpl string, data pongo2.Context) (string, error) {
	log.Println("Writing template...")

	t, err := pongo2.FromString(tmpl)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	output, err := t.Execute(data)
	if err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return reformatTemplate(output), nil
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

// isStreamGroup returns true if the label group has proto=tcp or proto=udp.
func isStreamGroup(dockerProxy, group string, labels map[string]string) bool {
	proto, ok := labels[dockerProxy+"."+group+".proto"]
	if !ok {
		return false
	}
	proto = strings.ToLower(proto)
	return proto == "tcp" || proto == "udp"
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

		// Skip TCP/UDP groups — handled by makeStreamEntryList
		if isStreamGroup(parts[0], group, ctr.Labels) {
			processed[group] = true
			continue
		}

		list = append(list, makeVirtualHostFromLabels(parts[0], group, ctr.Labels))
		processed[group] = true
	}

	return list
}

// makeStreamEntryList extracts TCP/UDP stream entries from a container's labels.
func makeStreamEntryList(ctr config.Container) []streamEntry {
	var list []streamEntry

	processed := make(map[string]bool)
	for key := range ctr.Labels {
		parts := strings.SplitN(key, ".", 3)
		if len(parts) != 3 {
			continue
		}
		dockerProxy := parts[0]
		group := parts[1]

		if processed[group] {
			continue
		}

		if !isStreamGroup(dockerProxy, group, ctr.Labels) {
			processed[group] = true
			continue
		}

		proto := strings.ToLower(ctr.Labels[dockerProxy+"."+group+".proto"])

		port := 0
		if p, ok := ctr.Labels[dockerProxy+"."+group+".port"]; ok {
			if v, err := strconv.Atoi(p); err == nil {
				port = v
			}
		}

		listenPort := port // default: listen on same port as container port
		if p, ok := ctr.Labels[dockerProxy+"."+group+".listen"]; ok {
			if v, err := strconv.Atoi(p); err == nil {
				listenPort = v
			}
		}

		if listenPort == 0 {
			log.Printf("Skipping stream group %q for container %q: no listen port", group, ctr.Name)
			processed[group] = true
			continue
		}
		if port == 0 {
			port = listenPort // if no explicit container port, use listen port
		}

		host := ctr.Labels[dockerProxy+"."+group+".host"] // optional for SNI

		list = append(list, streamEntry{
			ListenPort: listenPort,
			Port:       port,
			Protocol:   proto,
			Host:       host,
		})

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
		isValid := false

		for key, val := range ctr.Labels {
			parts := strings.SplitN(key, ".", 3)
			if len(parts) != 3 || parts[0] != "docker-proxy" {
				continue
			}

			// HTTP upstream: has a non-empty .host label
			if parts[2] == "host" && len(val) > 0 {
				isValid = true
				break
			}

			// Stream upstream: has .proto = tcp or udp
			if parts[2] == "proto" {
				proto := strings.ToLower(val)
				if proto == "tcp" || proto == "udp" {
					isValid = true
					break
				}
			}
		}

		// Check for VIRTUAL_HOST env var
		if !isValid {
			if _, ok := ctr.Env["VIRTUAL_HOST"]; ok {
				isValid = true
			}
		}

		if isValid {
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
