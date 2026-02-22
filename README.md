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
- `MANAGEMENT_SOCKET=/var/run/proxy/management.sock`
- `RENDERER=nginx`
- `PROXY_CONTAINER=docker-proxy`

`management` and `certs` are expected to be shared with `docker-proxy` (typically `external: true`).

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

## Renderer

The `nginx` renderer builds template context data from inspected containers:

| Template variable | Description |
|---|---|
| `ServerList` | HTTP server blocks (from `host` labels / `VIRTUAL_HOST`) |
| `UpstreamList` | HTTP upstream blocks with network addresses |
| `StreamPortList` | TCP/UDP listen directives with optional SNI maps |
| `StreamUpstreamList` | TCP/UDP upstream blocks |
| `ErrorPageData` | Base64-encoded JSON for the proxy landing/error pages |

The rendered output uses the `### STREAM_CONFIG ###` delimiter to separate HTTP and stream sections. The proxy management server splits these into `/etc/nginx/conf.d/default.conf` and `/etc/nginx/stream.d/default.conf`.

## Security notes

- The Docker socket is powerful; treat this container as privileged.
- Configuration generation is intentionally constrained by renderer behavior. Avoid running untrusted templates/inputs.