// Package web serves the REST API and an embedded single-page management UI.
// It is a thin HTTP layer: RTSP probing, MJPEG preview and ffmpeg snapshots are
// delegated to sibling packages, while configuration read/write and the device
// runtime snapshot come from the Backend implemented by main.
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Aiaid/onvif-proxy/internal/config"
	"github.com/Aiaid/onvif-proxy/internal/discovery"
)

// all:static embeds the whole UI tree, including the built bundle under
// static/dist (the "all:" prefix keeps files the default rules would skip).
//
//go:embed all:static
var staticFiles embed.FS

// Backend is implemented by main; it wires the web layer to the live runtime.
type Backend interface {
	// ConfigYAML returns the current config.yaml text verbatim.
	ConfigYAML() ([]byte, error)
	// ApplyConfig parses raw YAML and, unless dryRun, persists and hot-reloads
	// it. Validation/parse errors are returned verbatim for the UI to display.
	ApplyConfig(raw []byte, dryRun bool) error
	// Devices returns a snapshot of the running devices.
	Devices() []DeviceRuntime
	// Snapshot grabs a single JPEG frame from the given device/stream.
	Snapshot(ctx context.Context, dev *config.Device, streamName string) ([]byte, string, error)
	// DiscoveryLog returns the recent WS-Discovery interactions.
	DiscoveryLog() []discovery.LogEntry
	// Status returns coarse runtime status for the top status bar.
	Status() Status
}

// DeviceRuntime pairs a configured device with its running state.
type DeviceRuntime struct {
	Device  *config.Device
	Running bool
	Error   string
}

// Status is the payload of GET /api/status.
type Status struct {
	Version       string `json:"version"`
	AdvertiseIP   string `json:"advertise_ip"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	FFmpeg        bool   `json:"ffmpeg"`
}

// Server owns the HTTP mux and per-device preview concurrency bookkeeping.
type Server struct {
	cfg     config.WebConfig
	backend Backend
	handler http.Handler

	previewMu  sync.Mutex
	previewSem map[string]int // device uuid -> active preview count

	// configMu serializes every read-modify-write config operation across the
	// REST and MCP surfaces, preventing concurrent device edits from silently
	// overwriting one another.
	configMu sync.Mutex
}

const maxPreviewPerDevice = 2

const (
	maxConfigBodyBytes = 1 << 20
	maxDeviceBodyBytes = 1 << 20
	maxProbeBodyBytes  = 1 << 16
	maxMCPBodyBytes    = 1 << 20
)

var errRequestBodyTooLarge = errors.New("request body exceeds size limit")

// readRequestBody reads at most max bytes and explicitly detects overflow.
// io.LimitReader(max) alone can silently truncate a valid JSON/YAML prefix.
func readRequestBody(r *http.Request, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, errRequestBodyTooLarge
	}
	return body, nil
}

func decodeJSONBody(r *http.Request, max int64, dst any) error {
	body, err := readRequestBody(r, max)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

func writeBodyError(w http.ResponseWriter, label string, err error) {
	if errors.Is(err, errRequestBodyTooLarge) {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body too large", err.Error())
		return
	}
	writeErr(w, http.StatusBadRequest, label, err.Error())
}

type webConfigProvider interface {
	WebConfig() config.WebConfig
}

// New builds a Server. Basic authentication is evaluated for every request so
// a Backend that implements webConfigProvider can rotate credentials on a hot
// reload without restarting the listening socket.
func New(cfg config.WebConfig, backend Backend) *Server {
	s := &Server{
		cfg:        cfg,
		backend:    backend,
		previewSem: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	mux.HandleFunc("GET /api/devices", s.handleDevices)
	mux.HandleFunc("POST /api/devices", s.handleAddDevice)
	mux.HandleFunc("PUT /api/devices/{uuid}", s.handleEditDevice)
	mux.HandleFunc("DELETE /api/devices/{uuid}", s.handleDeleteDevice)
	mux.HandleFunc("POST /api/test/rtsp", s.handleTestRTSP)
	mux.HandleFunc("POST /api/test/streaminfo", s.handleTestStreamInfo)
	mux.HandleFunc("GET /api/test/snapshot", s.handleTestSnapshot)
	mux.HandleFunc("GET /api/preview", s.handlePreview)
	mux.HandleFunc("POST /api/test/onvif", s.handleTestONVIF)
	mux.HandleFunc("GET /api/discovery/log", s.handleDiscoveryLog)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	// MCP endpoint: the SDK's Streamable HTTP handler owns GET/POST/DELETE
	// semantics, so it is mounted without a method filter.
	mux.Handle("/mcp", limitBody(s.mcpHandler(), maxMCPBodyBytes))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	// Serve the built bundle (dist/app.js, dist/app.css) straight from the
	// embedded FS; the sub-FS strips the "static/" prefix so /dist/* maps to
	// static/dist/*.
	if sub, err := fs.Sub(staticFiles, "static"); err == nil {
		mux.Handle("GET /dist/", http.FileServerFS(sub))
	}
	// Match the root path exactly ("/{$}"), not as a catch-all subtree: the
	// unrestricted "/mcp" route above matches all methods, and a bare "GET /"
	// would overlap it on GET /mcp with neither pattern more specific, which
	// ServeMux rejects as a conflict. handleIndex only ever served "/" anyway.
	mux.HandleFunc("GET /{$}", s.handleIndex)

	s.handler = s.withAuth(mux)
	return s
}

func limitBody(next http.Handler, max int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", s.cfg.Port))
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve runs the web server on an already-bound listener. This lets main
// surface a port conflict synchronously before reporting startup success.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.cfg.Port),
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// withAuth enforces HTTP Basic auth across every route when configured.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg
		if provider, ok := s.backend.(webConfigProvider); ok {
			cfg = provider.WebConfig()
		}
		if cfg.Username == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		gotUser, wantUser := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(cfg.Username))
		gotPass, wantPass := sha256.Sum256([]byte(pass)), sha256.Sum256([]byte(cfg.Password))
		valid := subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) &
			subtle.ConstantTimeCompare(gotPass[:], wantPass[:])
		if !ok || valid != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="onvif-proxy"`)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ui unavailable", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Status())
}

func (s *Server) handleDiscoveryLog(w http.ResponseWriter, _ *http.Request) {
	log := s.backend.DiscoveryLog()
	if log == nil {
		log = []discovery.LogEntry{}
	}
	writeJSON(w, http.StatusOK, log)
}

// findDevice returns the running device with the given uuid, or nil.
func (s *Server) findDevice(uuid string) *config.Device {
	for _, dr := range s.backend.Devices() {
		if dr.Device != nil && dr.Device.UUID == uuid {
			return dr.Device
		}
	}
	return nil
}

// ---- response helpers ------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr emits the uniform {"error","detail"} error envelope.
func writeErr(w http.ResponseWriter, status int, msg, detail string) {
	writeJSON(w, status, map[string]string{"error": msg, "detail": detail})
}
