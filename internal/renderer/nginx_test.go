package renderer

import (
	"strings"
	"testing"

	"github.com/christhomas/docker-config-gen/internal/config"
)

// streamTemplate is the shape the real nginx.template uses for stream ports: an SNI
// map plus `ssl_preread on` when the port is TLS, a plain proxy_pass when it is not.
const streamTemplate = `
{% if StreamPortList %}
{% for port in StreamPortList %}
{% if port.HasSNI %}
map $ssl_preread_server_name $stream_backend_{{ port.ListenPort }} {
	{% for entry in port.SNIEntries %}{{ entry.Host }} {{ entry.Upstream }};
	{% endfor %}default {{ port.DefaultUpstream }};
}
server {
	listen {{ port.ListenPort }};
	ssl_preread on;
	proxy_pass $stream_backend_{{ port.ListenPort }};
}
{% else %}
server {
	listen {{ port.ListenPort }};
	proxy_pass {{ port.DefaultUpstream }};
}
{% endif %}
{% endfor %}
{% endif %}
`

func container(name string, labels map[string]string) config.Container {
	return config.Container{
		ID:     name + "-id",
		Name:   name,
		Labels: labels,
		Networks: config.NetworkMap{
			"net1": {Name: "testnet", IPAddress: "10.0.0.2", ID: "net1"},
		},
	}
}

// A plain-text port must NOT get ssl_preread. nginx waits for a client TLS
// ClientHello when that is on, but SMTP, IMAP and POP3 in plaintext have the SERVER
// greet first — so both ends wait and the connection hangs. Every stream port used
// to be treated as SNI-routed, which made those protocols unusable through the proxy
// while TLS-first ports worked.
func TestStreamPortWithoutSSLHasNoPreread(t *testing.T) {
	out, err := Nginx(streamTemplate, []config.Container{
		container("smtp-gateway", map[string]string{
			"docker-proxy.smtp.proto": "tcp",
			"docker-proxy.smtp.port":  "25",
			"docker-proxy.smtp.host":  "mail.localhost",
		}),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out.Config, "ssl_preread") {
		t.Errorf("plain port emitted ssl_preread:\n%s", out.Config)
	}
	if strings.Contains(out.Config, "ssl_preread_server_name") {
		t.Errorf("plain port emitted an SNI map, which cannot match without TLS:\n%s", out.Config)
	}
	if !strings.Contains(out.Config, "listen 25;") {
		t.Errorf("port 25 was not rendered at all:\n%s", out.Config)
	}
}

func TestStreamPortWithSSLGetsSNIRouting(t *testing.T) {
	out, err := Nginx(streamTemplate, []config.Container{
		container("imap-gateway", map[string]string{
			"docker-proxy.imaps.proto": "tcp",
			"docker-proxy.imaps.port":  "993",
			"docker-proxy.imaps.host":  "mail.localhost",
			"docker-proxy.imaps.ssl":   "true",
		}),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"ssl_preread on;", "ssl_preread_server_name", "mail.localhost", "listen 993;"} {
		if !strings.Contains(out.Config, want) {
			t.Errorf("missing %q in:\n%s", want, out.Config)
		}
	}
}

// Opt-in means anything that is not a clear yes is a no, so a typo fails towards the
// safe path rather than hanging a plain-text port.
func TestSSLLabelAcceptsOnlyClearAffirmatives(t *testing.T) {
	for _, c := range []struct {
		value string
		want  bool
	}{
		{"true", true}, {"TRUE", true}, {"yes", true}, {"on", true}, {"1", true},
		{"false", false}, {"no", false}, {"", false}, {"maybe", false}, {"tru", false},
	} {
		out, err := Nginx(streamTemplate, []config.Container{
			container("gw", map[string]string{
				"docker-proxy.x.proto": "tcp",
				"docker-proxy.x.port":  "9000",
				"docker-proxy.x.host":  "h.localhost",
				"docker-proxy.x.ssl":   c.value,
			}),
		})
		if err != nil {
			t.Fatalf("render %q: %v", c.value, err)
		}
		got := strings.Contains(out.Config, "ssl_preread")
		if got != c.want {
			t.Errorf("ssl=%q → ssl_preread=%v, want %v", c.value, got, c.want)
		}
	}
}

// An HTTP service is unaffected: it has no `proto` label, so it never becomes a
// stream port regardless of this change.
func TestHTTPServiceIsNotAStreamPort(t *testing.T) {
	out, err := Nginx(streamTemplate, []config.Container{
		container("api", map[string]string{
			"docker-proxy.api.host": "app.localhost",
			"docker-proxy.api.port": "8080",
			"docker-proxy.api.path": "/api",
		}),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out.Config, "listen 8080;") {
		t.Errorf("an HTTP service was rendered as a stream port:\n%s", out.Config)
	}
}
