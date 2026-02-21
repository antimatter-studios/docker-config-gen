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

## Security notes

- The Docker socket is powerful; treat this container as privileged.
- Configuration generation is intentionally constrained by renderer behavior. Avoid running untrusted templates/inputs.