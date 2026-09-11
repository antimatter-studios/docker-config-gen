# docker-config-gen

`docker-config-gen` is a Docker event watcher + configuration generator.

It connects to the Docker daemon via the read-only Docker socket, inspects containers, and renders configuration changes using a renderer (currently: `nginx`).

## How it works

- **[Docker events]** Watches the Docker socket for container lifecycle events.
- **[Inspection]** Reads container environment and labels to discover routing metadata.
- **[Rendering]** Uses `RENDERER` to generate output (for nginx reverse-proxying).
- **[Management socket]** Talks to the proxy over a Unix domain socket on a shared Docker volume.

## Docker Compose (recommended)

This repo ships a `docker-compose.yml` that runs config-gen with the correct socket + shared volumes. The important parts are:

- `- /var/run/docker.sock:/var/run/docker.sock:ro`
- `- management:/var/run/proxy`
- `- certs:/etc/nginx/certs` and `- <ca>:/etc/docker-config-gen/ca:ro` (only for [HTTPS](#https))
- `MANAGEMENT_SOCKET=/var/run/proxy/management.sock`
- `RENDERER=nginx`
- `PROXY_CONTAINER=docker-proxy`
- `CA_DIR=/etc/docker-config-gen/ca` and `CERTS_DIR=/etc/nginx/certs` (the defaults)

`management` and `certs` are expected to be shared with `docker-proxy` (typically `external: true`). Mount `certs` at the same path in both containers: the paths written here go into the proxy's configuration.

## Container label format

Containers opt into proxying via **Docker labels** (preferred) or the legacy `VIRTUAL_HOST` environment variable.

### HTTP virtual hosts

```yaml
labels:
  docker-proxy.<group>.host: myapp.localhost
  docker-proxy.<group>.port: "3000"        # default: 80
  docker-proxy.<group>.protocol: http      # default: http (also: https)
  docker-proxy.<group>.path: /             # default: /
```

`<group>` is an arbitrary name that groups related labels. Multiple groups per container create multiple routing rules.

### TCP/UDP streams

For raw TCP or UDP traffic, set `proto` instead of `host`:

```yaml
labels:
  docker-proxy.<group>.proto: tcp          # or: udp
  docker-proxy.<group>.port: "5432"        # container port
  docker-proxy.<group>.listen: "5432"      # proxy listen port (default: same as port)
  docker-proxy.<group>.host: db.example.com  # optional: enables SNI routing
```

When `host` is specified alongside `proto`, nginx uses TLS SNI to route multiple containers on the same listen port by hostname. Without `host`, traffic is forwarded to the default upstream (first container registered for that port).

### Legacy environment variables

```yaml
environment:
  VIRTUAL_HOST: myapp.localhost
  VIRTUAL_PORT: "3000"
  VIRTUAL_PROTO: http
```

## HTTPS

When a CA is mounted at `CA_DIR` (`ca.crt` and `ca.key`, PEM), every HTTP host is also served over HTTPS. There is no label for it: the host name is the certificate's name. For each host the generator keeps `<host>.crt` and `<host>.key` in `CERTS_DIR`, issuing them on first sight and replacing them when fewer than 30 days remain, when the CA changes, or when they don't match the host. Certificates are valid for 825 days, the most Apple platforms accept.

A host the CA cannot vouch for, such as a name outside the CA's name constraints or an nginx regular expression, is logged once and stays HTTP-only. With no CA mounted, every host is HTTP-only, as before.

The CA is created and trusted on the developer's machine by the orchestrator (ddt does this on install); this container only reads it.

## Renderer

The `nginx` renderer builds template context data from inspected containers:

| Template variable | Description |
|---|---|
| `ServerList` | HTTP server blocks (from `host` labels / `VIRTUAL_HOST`): `Host`, `Locations`, and `Certificate` / `CertificateKey` when the host has HTTPS |
| `UpstreamList` | HTTP upstream blocks with network addresses |
| `StreamPortList` | TCP/UDP listen directives with optional SNI maps |
| `StreamUpstreamList` | TCP/UDP upstream blocks |
| `ErrorPageData` | Base64-encoded JSON for the proxy landing/error pages |

The rendered output uses the `### STREAM_CONFIG ###` delimiter to separate HTTP and stream sections. The proxy management server splits these into `/etc/nginx/conf.d/default.conf` and `/etc/nginx/stream.d/default.conf`.

## Security notes

- The Docker socket is powerful; treat this container as privileged.
- The CA key is mounted here read-only and nowhere else: the proxy only ever sees the per-host certificates. Keep the CA restricted to development names (name constraints), so the key cannot vouch for real sites.
- Configuration generation is intentionally constrained by renderer behavior. Avoid running untrusted templates/inputs.