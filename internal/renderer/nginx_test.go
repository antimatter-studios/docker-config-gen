package renderer

import (
	"os"
	"strings"
	"testing"

	"github.com/christhomas/docker-config-gen/internal/config"
	"github.com/flosch/pongo2/v6"
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

// ddt publishes a TCP port by running a sidecar container that forwards the host port
// into nginx. It marks that container with docker-proxy.sidecar=true, plus
// docker-proxy.sidecar.port and .proto — which parse as a service group called
// "sidecar", so the generator registered the sidecar as an upstream FOR ITS OWN PORT.
//
// nginx then forwarded the port back to the thing that had just forwarded it in. Ports
// 465 and 995 hung outright; 993 survived only because the real gateway happened to win
// the same race. It went unnoticed while every stream port did SNI routing, because the
// real service matched by hostname and only non-SNI traffic fell into the loop.
func TestSidecarIsNeverItsOwnUpstream(t *testing.T) {
	out, err := Nginx(streamTemplate, []config.Container{
		container("docker-proxy-sidecar-tcp-465", map[string]string{
			"docker-proxy.sidecar":       "true",
			"docker-proxy.sidecar.port":  "465",
			"docker-proxy.sidecar.proto": "tcp",
		}),
		container("smtp-gateway", map[string]string{
			"docker-proxy.submissions.proto": "tcp",
			"docker-proxy.submissions.port":  "465",
			"docker-proxy.submissions.host":  "mail.localhost",
			"docker-proxy.submissions.ssl":   "true",
		}),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out.Config, "sidecar") {
		t.Errorf("a sidecar container was used as an upstream:\n%s", out.Config)
	}
	if !strings.Contains(out.Config, "smtp-gateway") {
		t.Errorf("the real service is not the upstream for its port:\n%s", out.Config)
	}
}

// The same container, with no other service claiming the port: the port must simply not
// be rendered, rather than rendered as a loop back into nginx.
func TestSidecarAloneRendersNoStreamPort(t *testing.T) {
	out, err := Nginx(streamTemplate, []config.Container{
		container("docker-proxy-sidecar-tcp-995", map[string]string{
			"docker-proxy.sidecar":       "true",
			"docker-proxy.sidecar.port":  "995",
			"docker-proxy.sidecar.proto": "tcp",
		}),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out.Config, "listen 995;") {
		t.Errorf("port 995 was rendered with only a sidecar to point at:\n%s", out.Config)
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

// TestMaxBodySize renders the REAL template from the docker-proxy repository rather
// than a fixture, because the point of this pair of changes is that the generator and
// the template agree: the generator supplies MaxBodySize and reads the per-location
// label, the template emits both. A fixture would prove neither.
func TestMaxBodySize(t *testing.T) {
	tmpl, err := os.ReadFile("../../../docker-proxy/nginx.template")
	if err != nil {
		t.Skipf("real template not available beside this checkout: %v", err)
	}

	t.Run("proxy-wide default is emitted", func(t *testing.T) {
		out, err := Nginx(string(tmpl), []config.Container{
			container("api", map[string]string{
				"docker-proxy.api.host": "app.localhost",
				"docker-proxy.api.port": "8080",
			}),
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		// nginx's own default is 1m, and a proxy that silently caps uploads at 1m
		// turns a 12MB attachment into a 413 that looks like an application bug.
		if !strings.Contains(out.Config, "client_max_body_size 512m;") {
			t.Errorf("default upload limit missing from the rendered config")
		}
	})

	t.Run("PROXY_MAX_BODY_SIZE overrides the default", func(t *testing.T) {
		t.Setenv("PROXY_MAX_BODY_SIZE", "64m")
		out, err := Nginx(string(tmpl), []config.Container{
			container("api", map[string]string{"docker-proxy.api.host": "app.localhost"}),
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if !strings.Contains(out.Config, "client_max_body_size 64m;") {
			t.Errorf("env override not applied")
		}
	})

	// The image tags these run under are floating (`:latest`), so a newer template can
	// be rendered by an older generator that has never heard of MaxBodySize. The
	// directive has to be guarded, or that combination emits it with no value and nginx
	// refuses to load the config — the proxy would not start at all.
	t.Run("an older generator still renders loadable config", func(t *testing.T) {
		tpl, err := pongo2.FromString(string(tmpl))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		out, err := tpl.Execute(pongo2.Context{}) // nothing supplied, as an old generator would
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		for _, line := range strings.Split(out, "\n") {
			// Skip comments: the template documents this very failure mode, and
			// matching the whole output caught its own explanation.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if trimmed == "client_max_body_size ;" || trimmed == "client_max_body_size;" {
				t.Errorf("emitted a valueless directive: %q", trimmed)
			}
		}
	})

	t.Run("a label overrides it for one location only", func(t *testing.T) {
		out, err := Nginx(string(tmpl), []config.Container{
			container("uploads", map[string]string{
				"docker-proxy.up.host":          "app.localhost",
				"docker-proxy.up.path":          "/uploads",
				"docker-proxy.up.port":          "8080",
				"docker-proxy.up.max_body_size": "2g",
			}),
			container("api", map[string]string{
				"docker-proxy.api.host": "app.localhost",
				"docker-proxy.api.path": "/api",
				"docker-proxy.api.port": "8080",
			}),
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if !strings.Contains(out.Config, "client_max_body_size 2g;") {
			t.Errorf("per-location override missing:\n%s", out.Config)
		}
		// The override belongs to its own location, not to the whole server: the
		// /api location must not inherit 2g.
		uploads := strings.Index(out.Config, `location "/uploads"`)
		api := strings.Index(out.Config, `location "/api"`)
		override := strings.Index(out.Config, "client_max_body_size 2g;")
		if uploads < 0 || api < 0 || override < 0 {
			t.Fatalf("expected both locations and the override:\n%s", out.Config)
		}
		if !(override > uploads && (api < uploads || override < api)) {
			t.Errorf("the 2g override is not inside the /uploads location:\n%s", out.Config)
		}
	})
}

// fakeCerts hands out certificate paths for the hosts it holds, as an issuer would once
// it had issued them. The value is whether the files were just written.
type fakeCerts map[string]bool

func (f fakeCerts) Certificate(host string) (string, string, bool, bool) {
	changed, ok := f[host]
	if !ok {
		return "", "", false, false
	}
	return "/etc/nginx/certs/" + host + ".crt", "/etc/nginx/certs/" + host + ".key", changed, true
}

// serverBlock returns the rendered server block for host: from its server_name to the
// next server block.
func serverBlock(t *testing.T, config, host string) string {
	t.Helper()
	start := strings.Index(config, "server_name "+host+";")
	if start < 0 {
		t.Fatalf("no server block for %s:\n%s", host, config)
	}
	block := config[start:]
	if end := strings.Index(block, "server {"); end >= 0 {
		block = block[:end]
	}
	return block
}

// TestHTTPS renders the REAL template, for the same reason as TestMaxBodySize: the
// generator supplies each host's certificate, and the template has to turn it into an
// HTTPS listener for that host alone.
func TestHTTPS(t *testing.T) {
	tmpl, err := os.ReadFile("../../../docker-proxy/nginx.template")
	if err != nil {
		t.Skipf("real template not available beside this checkout: %v", err)
	}
	containers := []config.Container{
		container("app", map[string]string{"docker-proxy.app.host": "app.localhost", "docker-proxy.app.port": "8080"}),
		container("other", map[string]string{"docker-proxy.other.host": "other.localhost", "docker-proxy.other.port": "8080"}),
	}

	t.Run("a host with a certificate listens on 443 with it", func(t *testing.T) {
		out, err := NginxWithCertificates(string(tmpl), containers, fakeCerts{"app.localhost": false})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		block := serverBlock(t, out.Config, "app.localhost")
		for _, want := range []string{
			"listen 443 ssl;",
			"ssl_certificate /etc/nginx/certs/app.localhost.crt;",
			"ssl_certificate_key /etc/nginx/certs/app.localhost.key;",
		} {
			if !strings.Contains(block, want) {
				t.Errorf("missing %q in the app.localhost block:\n%s", want, block)
			}
		}
	})

	t.Run("a host without one stays HTTP-only", func(t *testing.T) {
		out, err := NginxWithCertificates(string(tmpl), containers, fakeCerts{"app.localhost": false})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if block := serverBlock(t, out.Config, "other.localhost"); strings.Contains(block, "443") {
			t.Errorf("a host with no certificate listens on 443:\n%s", block)
		}
	})

	// Without a catch-all, nginx answers an unknown name on 443 with whichever host's
	// certificate comes first, and the client sees someone else's host name.
	t.Run("unknown names have the handshake refused", func(t *testing.T) {
		out, err := Nginx(string(tmpl), containers)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if !strings.Contains(out.Config, "listen 443 ssl default_server;") || !strings.Contains(out.Config, "ssl_reject_handshake on;") {
			t.Errorf("no refusing catch-all on 443:\n%s", out.Config)
		}
	})

	t.Run("a newly written certificate is reported, so nginx reloads", func(t *testing.T) {
		out, err := NginxWithCertificates(string(tmpl), containers, fakeCerts{"app.localhost": true})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if !out.CertificatesChanged {
			t.Error("a written certificate was not reported")
		}
		out, err = NginxWithCertificates(string(tmpl), containers, fakeCerts{"app.localhost": false})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if out.CertificatesChanged {
			t.Error("an unchanged certificate was reported as written")
		}
	})

	// The image tags are floating, so this template can be rendered by a generator from
	// before certificates existed. Its servers have no Certificate field, and every host
	// must then stay HTTP-only rather than name a file that isn't there.
	t.Run("an older generator leaves every host HTTP-only", func(t *testing.T) {
		tpl, err := pongo2.FromString(string(tmpl))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		type oldServer struct {
			Host      string
			Locations []config.Location
		}
		out, err := tpl.Execute(pongo2.Context{"ServerList": []oldServer{{Host: "app.localhost"}}})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if !strings.Contains(out, "server_name app.localhost;") {
			t.Fatalf("the host was not rendered:\n%s", out)
		}
		if strings.Contains(out, "ssl_certificate") {
			t.Errorf("an older generator's host was given a certificate directive:\n%s", out)
		}
	})
}
