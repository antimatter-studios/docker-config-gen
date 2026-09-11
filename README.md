# docker-config-gen

`docker-config-gen` is a Docker event watcher + configuration generator.

It connects to the Docker daemon through the Docker socket, inspects containers, and renders configuration changes using a renderer (currently: `nginx`). It also creates the sidecar containers that publish TCP/UDP ports, so it needs full access to the socket.

## How it works

- **[Docker events]** Watches the Docker socket for container lifecycle events.
- **[Inspection]** Reads container environment and labels to discover routing metadata.
- **[Rendering]** Uses `RENDERER` to generate output (for nginx reverse-proxying).
- **[Management socket]** Talks to the proxy over a Unix domain socket on a shared Docker volume.

## Running it

[ddt](https://github.com/antimatter-studios/docker-dev-tools) runs it beside the proxy (`ddt proxy start`), with the Docker socket, the shared volumes and, for HTTPS, its CA. To try a local build, run `chore image:build`, then point a ddt that has `ddt proxy config-gen-image` at it (`ddt proxy config-gen-image ghcr.io/antimatter-studios/docker-config-gen:dev`) and `ddt proxy restart`.

Running it by hand means reproducing that setup:

- the Docker socket, `/var/run/docker.sock`
- the management volume at `/var/run/proxy`, shared with docker-proxy
- the certs volume at `/etc/nginx/certs`, mounted at the same path in docker-proxy (the paths written here go into its configuration), and for [HTTPS](#https) the CA at `/etc/docker-config-gen/ca`, read-only
- `MANAGEMENT_SOCKET=/var/run/proxy/management.sock`, `RENDERER=nginx`, `PROXY_CONTAINER=<the proxy's container name>`, and optionally `CA_DIR` and `CERTS_DIR` (defaults `/etc/docker-config-gen/ca` and `/etc/nginx/certs`)

## Developing it

```bash
chore test          # unit tests; the real-template tests need docker-proxy checked out beside this repo
chore lint
chore image:smoke   # build the image and check the binary in it starts, and fails legibly without a socket
```

CI runs the same, and a pull request merges itself once CI passes.

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

When a CA is mounted at `CA_DIR` (`ca.crt` and `ca.key`, PEM), every HTTP host is also served over HTTPS. There is no label for it: the host name is the certificate's name. For each host the generator keeps `<host>.pem` in `CERTS_DIR`, holding the certificate and its key together so a renewal replaces both at once (a host name too long to be a file name is hashed). It is issued on first sight and replaced when fewer than 30 days remain, when the CA changes, or when it doesn't match the host. Certificates are valid for 825 days, the most Apple platforms accept.

A host the CA cannot vouch for, such as a name outside the CA's name constraints or an nginx regular expression, is logged once and stays HTTP-only. With no CA mounted, every host is HTTP-only, as before.

The CA is created by the orchestrator (ddt does this when the proxy starts); this container only reads it, once, when it starts, so restart it after replacing the CA (ddt does). Software that should verify the proxy's certificates trusts that CA directly.

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