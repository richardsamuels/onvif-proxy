# onvif-proxy

**English** | [简体中文](README.zh-CN.md)

[![build-and-push](https://github.com/Aiaid/onvif-proxy/actions/workflows/docker-publish.yml/badge.svg)](https://github.com/Aiaid/onvif-proxy/actions/workflows/docker-publish.yml)

An **RTSP → ONVIF virtual camera proxy** written in Go. It wraps any existing RTSP stream (IP cameras, NVR channels, Raspberry Pi, ffmpeg pushes, …) into a spec-compliant ONVIF Profile S virtual device that can be discovered and adopted by Unifi Protect, Synology Surveillance Station, Frigate, and other ONVIF clients.

> **Why another one?** The existing [daniela-hase/onvif-server](https://github.com/daniela-hase/onvif-server) and its fork [p10tyr/rtsp-to-onvif](https://github.com/p10tyr/rtsp-to-onvif) are built on the Node.js `soap` library + WSDL. Any method not explicitly registered throws a malformed bare HTTP 500 (in practice `GetCapabilities`, `GetScopes`, and `GetNetworkInterfaces` all 500), which makes some clients mark the device as incompatible. This project is a from-scratch Go rewrite that implements the full mandatory method set with standard fault semantics, plus a built-in web UI for configuration and testing.

## Features

- **Full ONVIF Profile S mandatory coverage** — every mandatory Device and Media service method is implemented; unimplemented optional methods return a spec-compliant `ter:ActionNotSupported` SOAP fault instead of a bare 500.
- **Multiple media profiles per device** — not just a high/low pair: define any number of named streams (`main` / `sub` / `mobile` / …), each exposed as its own ONVIF profile with its own encoder configuration and stream URI.
- **Honest codec metadata** — each profile declares its configured H.264, JPEG/MJPEG, or MPEG-4 encoder consistently across profiles, options, and guaranteed-instance counts. H.265/HEVC is rejected because ONVIF Profile S does not define an H.265 encoder type.
- **Snapshot support** — `GetSnapshotUri` works out of the box: pass through the real camera's HTTP snapshot URL when it has one, or let the built-in ffmpeg grabber pull a JPEG frame straight from the RTSP stream (with short-TTL caching). This fills a gap left open by the upstream projects.
- **WS-Discovery** — multicast discovery on UDP 3702 with Hello / Bye / ProbeMatches, so clients find the virtual devices automatically.
- **Zero-transcode RTSP proxy** — a per-device TCP passthrough proxy forwards the stream bytes untouched to the real camera; no decoding, no CPU cost. ffmpeg is only used for snapshots and UI previews.
- **Web UI** — besides the YAML config file, a small embedded web backend lets you edit the config online, probe RTSP connectivity (native RTSP client with Digest/Basic auth and SDP parsing), grab snapshots, watch a live MJPEG preview, and run an ONVIF self-test against the virtual device itself.
- **Transactional hot reload** — configuration changes validate and reserve their listeners before being committed; bind or persistence failures restore the previous runtime and leave the old config file intact.
- **Docker-first deployment** — multi-stage build with ffmpeg baked in; macvlan compose example so each virtual device can get its own IP/MAC on your LAN.
- **Single binary, minimal dependencies** — `gopkg.in/yaml.v3` plus the official MCP `go-sdk`; SOAP messages are hand-written XML templates, no WSDL code generation.
- **MCP server built in** — `/mcp` (Streamable HTTP) exposes device management, RTSP probing, snapshots and the ONVIF self-test as MCP tools for AI clients such as Claude Code (see [docs/07-mcp.md](docs/07-mcp.md)).

## Documentation

Design docs (bilingual — each has a `.zh-CN.md` counterpart, e.g. [docs/01-architecture.zh-CN.md](docs/01-architecture.zh-CN.md)):

| Doc | Contents |
|-----|----------|
| [docs/01-architecture.md](docs/01-architecture.md) | Architecture, module layout, data flows, directory structure |
| [docs/02-onvif-spec.md](docs/02-onvif-spec.md) | ONVIF / SOAP / WS-Discovery / WSSE conformance design, method matrix, fault semantics |
| [docs/03-config.md](docs/03-config.md) | YAML config format and validation rules |
| [docs/04-web-api.md](docs/04-web-api.md) | Web backend REST API and UI design |
| [docs/05-deployment.md](docs/05-deployment.md) | Docker / macvlan deployment |
| [docs/06-internal-api.md](docs/06-internal-api.md) | Exported package interfaces and integration contracts |
| [docs/07-mcp.md](docs/07-mcp.md) | Built-in MCP server endpoint (`/mcp`), tool catalog, implementation contract |

## Quick start (Docker)

Multi-arch images (`linux/amd64` + `linux/arm64`, ffmpeg/ffprobe included) are published to both registries on every push to `main`:

| Registry | Image |
|----------|-------|
| GHCR | `ghcr.io/aiaid/onvif-proxy` |
| Docker Hub | `docker.io/anend/onvif-proxy` |

Tags: `latest` (main), `main`, `sha-<short>`, and `vX.Y.Z` on release tags.

```bash
# Linux host networking (simplest; multicast discovery works)
docker run -d --name onvif-proxy --network host \
  -v onvif-proxy-config:/config \
  ghcr.io/aiaid/onvif-proxy:latest

# Open the web UI and add devices through the form
open http://<host>:8080
```

No config file is needed for the first run — a default one is generated in the `onvif-proxy-config` volume, and devices can be added entirely through the web UI ("➕ 新增设备" form probes your RTSP URL and autofills resolution/fps). The image runs as the non-root user `10001:10001`; the named volume works without host permission changes. For a host bind mount, backups, macvlan (own IP/MAC per proxy), or bridge mode (Docker Desktop), see [docs/05-deployment.md](docs/05-deployment.md) and [compose.yaml](compose.yaml).

The default Web UI/MCP listener has no password. Keep it on a trusted network or set `ONVIF_WEB_USERNAME` and `ONVIF_WEB_PASSWORD` before exposing the host. Camera credentials remain visible through the authenticated full-config editor by design.

Global settings can also be overridden via environment variables (`-e ONVIF_WEB_USERNAME=admin -e ONVIF_WEB_PASSWORD=…` for web UI Basic auth, plus `ONVIF_ADVERTISE_IP`, `ONVIF_DISCOVERY`, `ONVIF_WEB_ENABLED`, `ONVIF_WEB_PORT`). Env beats YAML, applies in memory only, and is never written back to the mounted config file — see [docs/03-config.md](docs/03-config.md) §3.

### Docker env override example

`compose.yaml` (see the file in this repo for all three network modes):

```yaml
services:
  onvif-proxy:
    image: ghcr.io/aiaid/onvif-proxy:latest
    network_mode: host          # or macvlan / bridge, see docs/05-deployment.md
    restart: unless-stopped
    volumes:
      - onvif-config:/config
    environment:
      ONVIF_WEB_USERNAME: admin
      ONVIF_WEB_PASSWORD: change-me
      ONVIF_ADVERTISE_IP: "192.168.1.10"   # required in bridge mode
      ONVIF_WEB_PORT: "9090"

volumes:
  onvif-config:
```

### config.yaml example

A minimal single-device config (full reference: [docs/03-config.md](docs/03-config.md)):

```yaml
server:
  advertise_ip: ""     # empty = auto-detect
  discovery: true

web:
  enabled: true
  port: 8080

devices:
  - name: "Garage Camera"
    ports:
      soap: 8081
      rtsp: 8554
    streams:
      - name: main
        rtsp: "rtsp://user:pass@192.168.1.50:554/h264/ch1/main/av_stream"
        codec: h264
        width: 1920
        height: 1080
        framerate: 25
        bitrate: 2048
```

`uuid`, `mac`, and `serial` are auto-generated and written back on first save — omit them.

## Authentication model (RTSP vs ONVIF)

There are **two independent credential layers** — a frequent point of confusion:

| Layer | Where it lives | Verified by | Purpose |
|-------|----------------|-------------|---------|
| RTSP credentials | `user:pass@` inside `streams[].rtsp` | the **real camera** | pulling the stream |
| ONVIF credentials | `devices[].auth` (optional) | **onvif-proxy** (WSSE + HTTP Basic) | protecting SOAP and the snapshot HTTP endpoint |

Key behavior:

- The RTSP proxy is a plain TCP passthrough: **RTSP auth is end-to-end**. `GetStreamUri` never exposes credentials; the ONVIF client must authenticate against the real camera with the **camera's own RTSP credentials**.
- The `user:pass@` in the config is only used by onvif-proxy itself (snapshots, previews, probing) and is never handed to ONVIF clients.
- When `devices[].auth` is configured, SOAP uses WSSE and the snapshot URL uses the same pair through HTTP Basic. Basic auth does not encrypt transport, so use a trusted LAN or TLS termination.
- Omitting `devices[].auth` means the ONVIF and snapshot endpoints accept any client (recommended only for trusted LANs).

**Unifi Protect tip:** Protect asks for a single username/password during adoption and uses it for *both* layers. Enter the real camera's RTSP credentials there, and either leave `devices[].auth` unset or set it to the exact same pair — never a different one.

## Status

- [x] Design documents
- [x] Core implementation (SOAP services, WS-Discovery, RTSP proxy)
- [x] Web backend and UI
- [x] Docker image and compose
- [ ] Verification against Unifi Protect

All packages have unit tests; the built-in ONVIF self-test passes end-to-end
(every mandatory method returns 200, unknown methods return a proper
`ter:ActionNotSupported` fault).

## License

[MIT](LICENSE)
