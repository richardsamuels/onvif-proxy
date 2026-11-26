# 06 · Inter-Package Interface Contract (Implementation Alignment Baseline)

**English** | [简体中文](06-internal-api.zh-CN.md)

> This document is the **signature contract** used while developing the packages in parallel. Implementations must match the exported signatures here exactly; if an implementation reveals that the contract is unreasonable, update this document before making the change. `internal/config` is already implemented; the remaining packages are built on top of it as the foundation.

Module path: `github.com/Aiaid/onvif-proxy`. Dependency rules: every package may import `internal/config` and the standard library; **adding new third-party dependencies is forbidden** (the only direct go.mod dependency is yaml.v3, with one exception: `internal/web` may import the official `github.com/modelcontextprotocol/go-sdk/mcp` to implement the `/mcp` endpoint — see the contract in `docs/07-mcp.md`); packages must not import one another beyond what is declared below.

## internal/config (already implemented, read-only reference)

```go
type Config struct { Server ServerConfig; Web WebConfig; Alarm AlarmConfig; Devices []*Device }
type AlarmConfig struct { Enabled bool; Port int; HoldSeconds *int; Poll *AlarmPollConfig } // yaml: alarm.enabled/port/hold_seconds/poll
type AlarmPollConfig struct { URL, Username, Password string; IntervalMS int } // yaml: url/username/password/interval_ms
func (a AlarmConfig) ListenPort() int // Port, or DefaultAlarmPort (8899) when 0
func (a AlarmConfig) Hold() int       // seconds; nil -> DefaultAlarmHoldSeconds (60), 0 = disabled
func (p AlarmPollConfig) Interval() time.Duration // IntervalMS, or DefaultAlarmPollIntervalMS (1000) when 0
func Load(path string) (*Config, error)          // read + validate + generate uuid/mac and persist
func Parse(data []byte) (*Config, error)         // strict parse + validate (for dry-run)
func Prepare(data []byte) (*Config, []byte, error) // parse + validate + generate identity; returns canonical YAML without writing
func Save(path string, cfg *Config) error        // atomic write
func WriteDefault(path string) error             // exclusive-create starter config with mode 0600
func ApplyEnvOverrides(cfg *Config) ([]string, error) // ONVIF_* env overrides (in-memory only, re-validated after overriding); returns the applied items for logging
func DetectAdvertiseIP() string

type Device struct { Name, UUID, MAC, Serial string; Ports Ports; Info Info;
                     Auth *Auth; Streams []*Stream; Snapshot Snapshot }
func (d *Device) PrimaryStream() *Stream
func (d *Device) StreamByName(name string) *Stream
func (d *Device) StreamByProfileToken(token string) *Stream
func (d *Device) ProxyPortFor(s *Stream) int
func (d *Device) ProxyBindings() []ProxyBinding  // {ListenPort int; Target string}
func (d *Device) SnapshotStream() *Stream
func (d *Device) SnapshotCacheSeconds() int

type Stream struct { Name, RTSP, Codec string; Width, Height, Framerate, Bitrate, ProxyPort int }
func (s *Stream) CodecName() string // canonical h264(default), jpeg, or mpeg4
func (s *Stream) TargetAddr() string             // "host:port" (default 554)
func (s *Stream) PathQuery() string              // "/path?query"
func (s *Stream) SourceURL() string              // full upstream URL including credentials
func (s *Stream) ProfileToken() string           // "profile_<name>"
func (s *Stream) EncoderToken() string           // "vec_<name>"
```

## internal/soap (SOAP 1.2 base layer)

```go
package soap

// Parsed request
type Request struct {
    Action    string            // localName of the Body's first child element, e.g. "GetStreamUri"
    Namespace string            // the namespace URI of that element
    Body      []byte            // the Body's innerxml (for extracting parameters)
    Security  *UsernameToken    // nil when there is no WSSE header
    To        string            // WS-Addressing To header (empty when absent); used to route PullPoint calls
}
type UsernameToken struct { Username, Password, PasswordType, Nonce, Created string }

func ParseRequest(body []byte) (*Request, error)
// Extracts the text of the first element in innerxml matching localName (e.g. ProfileToken)
func ExtractElement(inner []byte, localName string) (string, bool)

// WSSE PasswordDigest / PasswordText verification; clockSkew is the tolerated deviation for Created
func (t *UsernameToken) Verify(username, password string, clockSkew time.Duration) bool

// Response rendering: body is the already-rendered inner XML of the Body; the Envelope/namespaces are added uniformly by this layer
func Envelope(body string) []byte
// Same, with header (already-rendered inner XML of env:Header, e.g. <wsa:Action>); empty header = no env:Header
func EnvelopeWithHeader(header, body string) []byte
// Namespace constants include NSEvents (tev), NSWSNT (wsnt), NSWSA (wsa), NSTopics (tns1), NSWSTOP (wstop)

// Fault: writes to w (sets Content-Type and HTTP status code) and returns
// code: "Sender"/"Receiver"; subcode e.g. "ter:ActionNotSupported"
func WriteFault(w http.ResponseWriter, code, subcode, reason string)
// Convenience wrappers:
func WriteActionNotSupported(w http.ResponseWriter, action string) // Receiver, HTTP 500
func WriteNotAuthorized(w http.ResponseWriter)                     // Sender, HTTP 400
func WriteInvalidArg(w http.ResponseWriter, reason string)         // Sender, HTTP 400

func XMLEscape(s string) string
```

Envelope uniformly declares the namespace prefixes `env` (SOAP 1.2), `tds`, `trt`, `tt`, `ter`, `tev`, `wsnt`, `wsa`, `tns1`, `wstop`. The Fault structure contains Code/Subcode/Reason (xml:lang="en").

## internal/onvif (Device + Media + Events services, one HTTP server per device)

```go
package onvif // allowed imports: internal/config, internal/soap

type Options struct {
    AdvertiseIP  string // fallback used to construct the URI when the request has no Host header
    Version      string // firmware version shown to clients
    // Snapshot callback, injected by main (pass-through / ffmpeg / caching are all composed on the main side)
    SnapshotFunc func(ctx context.Context, streamName string) (data []byte, contentType string, err error)
}

func NewServer(dev *config.Device, opts Options) *Server
func (s *Server) Run(ctx context.Context) error // listens on dev.Ports.SOAP; shuts down gracefully when ctx is canceled
func (s *Server) Serve(ctx context.Context, ln net.Listener) error // serves an already-bound listener; releases pending PullMessages on shutdown
func (s *Server) SetMotion(active bool) // updates motion state; on change queues a "Changed" event on every live pull point (safe for concurrent use)
func (s *Server) Motion() bool          // current motion state

// Routes:
//   POST /onvif/device_service
//   POST /onvif/media_service
//   POST /onvif/event_service
//   POST /onvif/subscription/{id}               (PullPoint subscription manager)
//   GET  /onvif/snapshot?token=<profileToken>   (HTTP Basic required when dev.Auth is configured)
```

Behavioral requirements (see docs/02-onvif-spec.md for details): the method matrix follows sections 4/5/6 of document 02; `GetSystemDateAndTime` requires no authentication; when `dev.Auth == nil` everything is allowed through unauthenticated; PasswordDigest requires a fresh valid timestamp/nonce and rejects nonce replay; URI construction prefers `r.Host`; unknown methods → `soap.WriteActionNotSupported`. SOAP bodies are capped at 1 MiB and HTTP header/read/idle timeouts are bounded.

## internal/alarm (TVT / ENS XML alarm-server receiver)

```go
package alarm // allowed imports: standard library only

type Options struct {
    Hold     time.Duration                 // clear a channel not re-reported true for this long; 0 = never
    OnChange func(channel int, active bool) // called without internal locks for each transition; channel = 1-based motionAlarm id
}

func NewServer(opts Options) *Server
var ErrNoStatus error                          // Parse: well-formed document without an alarmStatusInfo element
func Parse(body []byte) (map[int]bool, error) // motionAlarm id -> state; bad entries skipped; malformed/empty XML -> error; no alarmStatusInfo -> ErrNoStatus
func (s *Server) Apply(motion map[int]bool)   // snapshot semantics: true = active, false/absent = inactive; transitions sorted by channel
func (s *Server) Active() []int               // sorted active channels
func (s *Server) Handler() http.Handler       // POST /SendAlarmStatus; other methods 405, other paths 404
func (s *Server) Serve(ctx context.Context, ln net.Listener) error // serves an already-bound listener and runs the hold-timeout loop (1s tick)

type PollOptions struct {
    URL                string        // device API base URL; requests go to URL + "/GetAlarmStatus"
    Username, Password string        // HTTP Basic; empty Username = no Authorization header
    Interval           time.Duration // <= 0 -> 1s
    Client             *http.Client  // nil -> a client with a 5s timeout
}
func (s *Server) Poll(ctx context.Context, opts PollOptions) error // polls until ctx is cancelled (returns ctx.Err()) and runs the hold-timeout loop; failed polls change no state
```

Wire format and state semantics: docs/08-alarm-server.md. Push: success (including an ErrNoStatus document, which is acknowledged but not applied) → 200 `<config status="success"/>`; malformed or >64 KiB body → 400 `<config status="failed" errorCode="2"/>`. No authentication. Poll: only an HTTP 200 status document is applied; failures are logged on the first failure and on recovery.

## internal/discovery (WS-Discovery)

```go
package discovery

type Device struct { UUID, Name, Hardware, XAddr string } // XAddr is the full URL
type LogEntry struct { Time time.Time; Remote, Kind, Detail string } // Kind: probe/hello/bye/match

func New(devices []Device) *Server
func (s *Server) Run(ctx context.Context) error  // binds :3702, joins the multicast group, sends Hello; sends Bye when ctx is canceled
func Listen() (*net.UDPConn, *net.UDPAddr, error) // synchronously binds and joins the multicast group
func (s *Server) Serve(ctx context.Context, conn *net.UDPConn, addr *net.UDPAddr) error
func (s *Server) SetDevices(devices []Device)    // hot reload: sends Bye/Hello for removals, additions, and metadata changes
func (s *Server) Log() []LogEntry                // last 50 entries, newest first
```

## internal/rtspproxy (TCP pass-through)

```go
package rtspproxy

// Listens on listenPort across all interfaces (not just 127.0.0.1), bidirectionally
// io.Copy to target ("host:port"). Closes the listener and all in-flight connections
// when ctx is canceled. Each proxy accepts at most 128 active connections and enforces
// a shared bidirectional idle timeout.
func Run(ctx context.Context, listenPort int, target string) error
func Serve(ctx context.Context, ln net.Listener, target string) error
```

## internal/rtsp (native RTSP probing client)

```go
package rtsp

type ErrKind string
const (
    ErrDialTimeout   ErrKind = "dial_timeout"
    ErrAuthFailed    ErrKind = "auth_failed"
    ErrNotFound      ErrKind = "not_found"
    ErrNoVideoTrack  ErrKind = "no_video_track"
    ErrProtocol      ErrKind = "protocol_error"
)
type Track struct { Type, Codec, Fmtp string }     // Type: "video"/"audio"
type Result struct {
    OK        bool
    Status    int           // final RTSP status code
    Auth      string        // "none"/"basic"/"digest"
    Server    string        // the Server header
    LatencyMS int64
    Tracks    []Track
    ErrKind   ErrKind       // valid when OK=false
    ErrDetail string
}
func Probe(ctx context.Context, rawURL string) *Result
// OPTIONS → DESCRIBE; on 401, supports Digest (MD5, RFC 7616) and Basic per WWW-Authenticate;
// SDP parsing of m=/a=rtpmap/a=fmtp; overall timeout of 5s (can be canceled early via ctx)
```

## internal/mediautil (ffmpeg wrapper)

```go
package mediautil

func Available() bool // whether ffmpeg is present on PATH
func ProbeAvailable() bool // whether ffprobe is present on PATH

// Grabs a single JPEG frame: -rtsp_transport tcp -frames:v 1; a summary of stderr is folded into error
func Grab(ctx context.Context, rtspURL string) ([]byte, error)

// TTL cache + singleflight (concurrent requests for the same key run only one fetch)
type Cache struct{ /* ... */ }
func NewCache(ttl time.Duration) *Cache
func (c *Cache) Get(key string, fetch func() ([]byte, error)) ([]byte, error)

// MJPEG preview: scaled down to maxWidth, at fps frame rate, muted, written to w as
// multipart/x-mixed-replace; the ffmpeg process group is killed as soon as the client
// disconnects (r.Context is canceled)
func ServeMJPEG(w http.ResponseWriter, r *http.Request, rtspURL string, maxWidth, fps int) error

// Summary of codec/resolution/frame rate for a single video stream (extracted via
// ffprobe). FPS is 0 when frame-rate information is unavailable; Bitrate is in kbps,
// and falls back to a measured pass-through estimate when ffprobe metadata doesn't
// declare it, or 0 if that still can't be obtained.
type StreamInfo struct {
    Codec   string // ffprobe codec_name
    Width   int
    Height  int
    FPS     int
    Bitrate int
}

// Runs ffprobe against rtspURL (-rtsp_transport tcp, selecting the first video stream);
// when bit_rate cannot be obtained, falls back to measuring the pass-through bitrate
// over a few seconds; the URL is passed only as an exec argument, never through a
// shell. A 15s timeout is applied when ctx has no deadline. Called by
// POST /api/test/streaminfo and the MCP get_stream_info tool.
func ProbeInfo(ctx context.Context, rtspURL string) (*StreamInfo, error)
```

## internal/web (REST API + embedded UI)

```go
package web // allowed imports: internal/config, internal/rtsp, internal/mediautil, internal/discovery

// Implemented by main
type Backend interface {
    ConfigYAML() ([]byte, error)
    ApplyConfig(raw []byte, dryRun bool) error   // Parse → (persist + hot reload); errors are returned to the frontend as-is
    Devices() []DeviceRuntime                    // snapshot of the currently running devices
    Snapshot(ctx context.Context, dev *config.Device, streamName string) ([]byte, string, error)
    DiscoveryLog() []discovery.LogEntry
    Status() Status
}
type DeviceRuntime struct { Device *config.Device; Running bool; Error string; Motion bool }
type Status struct {
    Version, AdvertiseIP string
    UptimeSeconds        int64
    FFmpeg               bool
}

func New(cfg config.WebConfig, backend Backend) *Server
func (s *Server) Run(ctx context.Context) error
func (s *Server) Serve(ctx context.Context, ln net.Listener) error // serves an already-bound listener
```

See docs/04-web-api.md for REST routes and semantics (`/api/config`, `/api/devices`, `/api/test/rtsp`, `/api/test/streaminfo`, `/api/test/snapshot`, `/api/preview`, `/api/test/onvif`, `/api/discovery/log`, `/api/status`, `/healthz`). Key points:

- Probing calls `rtsp.Probe` directly; preview calls `mediautil.ServeMJPEG` directly (concurrency capped at 2 per device, with the web layer managing the semaphore); snapshots go through `Backend.Snapshot` (caching/pass-through is composed on the main side);
- The **ONVIF self-test** is implemented inside the web package: it sends hand-written SOAP envelopes method by method to `dev.Ports.SOAP` (WSSE generated according to dev.Auth), returning `[{method, http_status, soap_fault, pass}]` per the method list in docs/04-web-api.md; a final call to a nonexistent method is appended, whose pass condition is that the response body is a well-formed Fault XML with subcode ActionNotSupported;
- Static UI: the Preact+TSX source lives in `internal/web/ui/`, bundled by esbuild into `static/dist/{app.js,app.css}` (committed to the repository); `index.html` is a thin shell; `go:embed all:static` embeds it into the binary (including `dist/`); `GET /dist/*` is served from the embedded FS via `http.FileServerFS`; `PUT /api/devices/{uuid}` edits a device (see docs/04-web-api.md); the `GET /api/devices` DTO additionally supplies `rtsp_port/auth_user` and per-stream source parameters to prefill the edit form;
- Basic authentication: applies to all routes when cfg.Username is non-empty. When the backend also implements `WebConfig() config.WebConfig`, credentials are read dynamically so username/password changes take effect without restarting the web listener.

## cmd/onvif-proxy (main, integration layer, implemented last)

Responsibilities: flag parsing (`-config`, default `./config.yaml`; `-version`) → `config.Load` → assemble the Manager (implements `web.Backend`; starts an onvif.Server + rtspproxy per device; discovery.Server; when `alarm.enabled`, an alarm.Server whose OnChange maps channel N to `devices[N-1]`'s `onvif.Server.SetMotion`, run with `Poll` when `alarm.poll` is set (no listener bound) and with `Serve` otherwise; snapshot function = pass-through or mediautil.Grab + Cache) → signal handling and transactional hot reload. Reload first prepares the candidate and synchronously pre-binds every SOAP/RTSP/discovery/alarm listener. A bind or persistence failure restores the old generation and leaves the old file/runtime usable; successful persistence uses a same-directory atomic rename with mode 0600. `web.username/password` reload live, while changing `web.enabled` or `web.port` is rejected as restart-required.
