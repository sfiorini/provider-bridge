# Deployment

Provider Bridge ships as a single static binary. It runs as a standalone
process, in a container, or (for edge use) as a Cloudflare Worker compiled to
WASM. This document covers the generic deployment modes first, then a
reference container deployment — read that section before running the
container.

> The infrastructure snippets here (reverse proxy, Compose, etc.) are examples.
> Adjust them for your environment.

## Standalone binary

### Build

```bash
go build -o providerbridge ./cmd/providerbridge
```

### Run

```bash
./providerbridge -config /path/to/config.yml
```

The `-config` flag is not optional in practice. When it is omitted, the loader
falls back to `$HOME/provider-bridge/config.yml`; if that file is missing it
*silently creates a starter config there* and starts. A bare `docker run`
without `-config` therefore writes a default config inside the container's
`$HOME` (`/home/nonroot/provider-bridge/config.yml`) instead of reading yours.

### systemd

```ini
[Unit]
Description=Provider Bridge
After=network.target

[Service]
ExecStart=/usr/local/bin/providerbridge -config /etc/provider-bridge/config.yml
Restart=always
RestartSec=5
User=providerbridge

[Install]
WantedBy=multi-user.target
```

### nginx reverse proxy

```nginx
server {
    listen 443 ssl;
    server_name providerbridge.example.com;
    location / {
        proxy_pass http://127.0.0.1:38440;
        proxy_set_header Host $host;
        proxy_buffering off;   # required for streaming responses
    }
}
```

## Docker

### Image (multi-stage)

The repository `Dockerfile` builds a static binary and copies it into a
**distroless, nonroot** image:

```dockerfile
FROM golang:1.27-bookworm AS builder

ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/providerbridge ./cmd/providerbridge

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/providerbridge /app/providerbridge
COPY config.example.yml /app/config.example.yml

EXPOSE 38440

USER nonroot:nonroot
ENTRYPOINT ["/app/providerbridge"]
CMD ["-config", "/config/config.yml", "-addr", "0.0.0.0:38440"]
```

Because the user is `nonroot`, the container runs as uid/gid **65532**. Anything
the process must write — `data/`, trace output, generated catalogs — has to be
owned by that uid/gid on the host.

### Compose

The checked-in [`docker-compose.example.yml`](../docker-compose.example.yml) is
the reference. The important parts are the explicit `-config` command and the
bind mounts:

```yaml
services:
  providerbridge:
    build: .
    image: providerbridge:local
    container_name: providerbridge
    restart: unless-stopped
    command:
      - -config
      - /config/config.yml
      - -addr
      - 0.0.0.0:38440
    ports:
      - "127.0.0.1:38440:38440"
    volumes:
      - ./config.yml:/config/config.yml:ro
      - ./data:/app/data
      - ./logs:/app/logs
      - ./trace:/app/trace
```

Before `docker compose up`: the image runs no `mkdir` for `/config` or
`/app/data` (distroless has no shell); those paths come from the compose bind
mounts, so the host-side mount sources must exist and be owned by the nonroot
uid/gid. Docker would otherwise create missing sources as `root`, which fails
the ownership check:

```bash
mkdir -p ./data ./logs ./trace
sudo chown -R 65532:65532 ./data
chmod 644 ./config.yml
```

## Reference container deployment

A reference deployment runs one container (name `providerbridge`, image
`providerbridge:local` or a published release image), published on port
`38440`.

### Gotchas that each caused a real incident

- **Distroless nonroot.** The image runs as uid/gid `65532`. `data/` must be
  owned `65532:65532`, otherwise startup fails with misleading config errors
  (a "permission denied" buried inside the diagnostic output).
- **`config.yml` mode `0644`.** The file must be readable by the nonroot user.
  A `0600` host file breaks the container with the same class of confusing
  error.
- **`-config` is required.** A bare `docker run` without it creates a default
  config at `$HOME/provider-bridge/config.yml` inside the container and ignores
  the mounted file. The Compose `command:` supplies it.
- **SQLite WAL siblings.** The live database is `data/provider-bridge.db`. With
  WAL enabled it also has a `provider-bridge.db-wal` and
  `provider-bridge.db-shm`. Copying only the main `.db` while the container is
  running **loses recent transactions**. Stop the container first (a clean close
  checkpoints the WAL, after which the main file is self-contained), then copy.
- **Web-search resolution is startup-only.** Any `web_search` config change
  requires `docker restart provider-bridge`.

### Updating a deployment

Pull, rebuild, restart, and wait for the health check:

```bash
git pull --ff-only
docker compose build
docker compose up -d
docker compose ps   # wait until the service reports healthy
```

## Cloudflare Workers (WASM)

The Worker entry point builds with TinyGo:

```bash
go build -o worker.wasm ./cmd/cloudflare
```

Wrangler tasks are defined at the repo root (`build`, `deploy`, `dev`). The D1
persistence provider is configured through the Worker binding
(`extensions.db_d1.config.binding`); the Worker injects the database before
extension init.

## Config management

- The config file is specified with `-config`. Passing it explicitly is
  recommended everywhere except the Wrangler dev flow.
- With persistence enabled, runtime edits go through the management API
  (`/api/v1/config/graph`). See [CONFIGURATION.md](CONFIGURATION.md) for the
  graph-vs-YAML model and the Console resource mapping.
- Persistence defaults to SQLite locally; Cloudflare deployments use D1.
