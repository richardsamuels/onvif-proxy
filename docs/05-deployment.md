# 05 · Deployment (Docker)

**English** | [简体中文](05-deployment.zh-CN.md)

## 1. Image

Multi-stage build; the final image = `alpine` + ffmpeg + a single binary, targeting a size < 120MB (ffmpeg accounts for most of it).

```dockerfile
# ---- build ----
FROM golang:1.26.5-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/onvif-proxy ./cmd/onvif-proxy

# ---- runtime ----
FROM alpine:3.20
ARG UID=10001
ARG GID=10001
RUN apk add --no-cache ffmpeg tzdata ca-certificates \
    && addgroup -S -g "${GID}" onvif-proxy \
    && adduser -S -D -H -u "${UID}" -G onvif-proxy onvif-proxy \
    && mkdir -p /config \
    && chown onvif-proxy:onvif-proxy /config
COPY --from=build /out/onvif-proxy /usr/local/bin/onvif-proxy
VOLUME /config
EXPOSE 8080
USER onvif-proxy:onvif-proxy
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["sh", "-ec", "kill -0 1"]
ENTRYPOINT ["onvif-proxy", "-config", "/config/config.yaml"]
```

Multi-arch: `linux/amd64` + `linux/arm64` (Raspberry Pi / NAS), built with `docker buildx` + GitHub Actions and published to GHCR. The builder and CI use the patched Go 1.26.5 toolchain. The application runs as the fixed non-root user `10001:10001`. The health check tests the PID 1 process rather than `/healthz`, so it still works when the Web UI is disabled or protected by Basic auth. It is a liveness check, not a readiness check for every configured ONVIF/RTSP listener.

### Environment variables

Global fields support env overrides (env > yaml, in-memory only, not written back to the mounted config.yaml; see `docs/03-config.md` §3 for the full semantics): `ONVIF_ADVERTISE_IP`, `ONVIF_DISCOVERY`, `ONVIF_WEB_ENABLED`, `ONVIF_WEB_PORT`, `ONVIF_WEB_USERNAME`, `ONVIF_WEB_PASSWORD`, and `CONFIG` (the config file path). Typical uses: setting Basic auth for the Web UI directly in the compose file, or specifying the host IP in bridge mode, without having to hand-write config.yaml beforehand.

## 2. Choosing a network mode (critical)

WS-Discovery relies on UDP multicast, which a plain bridge network cannot pass through. Three modes, in recommended order:

> The `compose.yaml` shipped at the repository root is the materialized version of the template below (host networking / mode B enabled by default); the `networks` top-level block for macvlan and the `ports` block for bridge are both kept as comments in their entirety — comment/uncomment the relevant section to switch modes as needed, without creating a separate file.

### Mode A: macvlan (recommended, cleanest for discovery + multiple devices)

The container gets its own IP/MAC on the host's subnet, so multicast, discovery, and adoption all work natively; what Unifi Protect sees is simply "another camera showing up on the network."

```yaml
# compose.yaml
services:
  onvif-proxy:
    image: ghcr.io/aiaid/onvif-proxy:latest
    restart: unless-stopped
    networks:
      lan:
        ipv4_address: 192.168.1.99      # must match advertise_ip in config.yaml
    volumes:
      - onvif-config:/config

networks:
  lan:
    driver: macvlan
    driver_opts:
      parent: eth0                       # the host's physical NIC
    ipam:
      config:
        - subnet: 192.168.1.0/24
          gateway: 192.168.1.1
          ip_range: 192.168.1.96/28      # small range reserved for the container

volumes:
  onvif-config:
```

Note: under macvlan, **the host itself cannot reach the container's IP by default** (a kernel limitation); access the management UI from another machine on the LAN, or add a macvlan shim interface.

### Mode B: host networking (Linux host, simplest for a single container)

```yaml
services:
  onvif-proxy:
    image: ghcr.io/aiaid/onvif-proxy:latest
    network_mode: host
    restart: unless-stopped
    volumes:
      - onvif-config:/config
    environment:                         # optional: env overrides for global config (see docs/03 §3)
      ONVIF_WEB_USERNAME: admin
      ONVIF_WEB_PASSWORD: ${WEB_PASSWORD}

volumes:
  onvif-config:
```

Multicast works; all ports are bound directly on the host, so plan around this to avoid conflicts. The same spot in compose.yaml also keeps a commented-out `ONVIF_WEB_PORT` override example (for changing the Web UI listen port) — uncomment it as needed, with no changes to config.yaml required.

### Mode C: bridge + manual add (discovery unavailable)

Map ports one by one (8080, plus each device's soap/rtsp ports), set `advertise_ip` to the host IP, and set `discovery: false`. On the client side, **manually add the ONVIF device by IP:port** (Unifi Protect supports manual entry). Suitable for Docker Desktop on Mac/Windows (whose virtualization layer can't pass multicast through anyway).

## 3. Multiple virtual devices and Unifi Protect

- Running multiple virtual devices on the same container/IP (with different soap ports) is allowed by the spec; Protect distinguishes devices by XAddr (host:port), and this has been verified to allow adopting them separately;
- If a client insists on "one IP per device," run multiple macvlan IPs, each with its own container instance (each instance with a trimmed-down config).

## 4. Upgrades and data

- The only persisted data is `/config/config.yaml` (including the auto-generated uuid/mac). The default `onvif-config` named volume inherits the image's non-root ownership, so an empty volume supports first-run config generation without any permission setup;
- Upgrading = swap the image and restart; persisting the uuid/mac ensures the client never mistakes the device for a new one;
- Backup = back up that single YAML file. For example: `docker cp onvif-proxy:/config/config.yaml ./config.yaml`;
- If you prefer a host bind mount for direct editing, run the container as your host UID/GID so atomic config rewrites remain writable:

  ```bash
  mkdir -p config
  docker run -d --name onvif-proxy --network host \
    --user "$(id -u):$(id -g)" \
    -v "$(pwd)/config:/config" \
    ghcr.io/aiaid/onvif-proxy:latest
  ```

  Alternatively, keep the image's default user and make the bind directory writable by UID/GID `10001:10001`. Do not make the directory world-writable; it may contain upstream camera credentials.

## 5. CI/CD (GitHub Actions, already implemented: `.github/workflows/docker-publish.yml`)

- **PR** → UI type/build checks + stale-bundle check + `go vet` + `go test -race` + `govulncheck` (test only, no image publish);
- **push to main / tag v\* / manual trigger** → test → amd64 + arm64 built in parallel on **native runners** (push-by-digest to GHCR, with gha cache) → a merge job combines the manifest list;
- Image target: `ghcr.io/aiaid/onvif-proxy` (via `GITHUB_TOKEN`, always available); when the repository has `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN` secrets configured, the same set of tags is automatically mirrored to Docker Hub `<user>/onvif-proxy` (imagetools copies the blobs cross-registry, no rebuild needed);
- Tag rules: branch name, `v*` versions (+ semver), short sha, and `latest` on main; the `VERSION` build-arg is injected into the binary (tag name or short sha);
- Workflow permissions default to read-only repository access, publishing jobs alone receive `packages: write`, and third-party actions are pinned to verified commit SHAs.

## 6. Running bare-metal (without Docker)

```bash
go build -o onvif-proxy ./cmd/onvif-proxy
./onvif-proxy -config config.yaml      # requires ffmpeg on PATH (used only for snapshots/preview)
```

Developing and debugging on macOS is entirely workable (multicast discovery works within the same LAN); Linux is recommended for production.
