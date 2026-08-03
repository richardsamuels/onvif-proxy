// Package onvif implements the ONVIF Device and Media services for a single
// virtual device. Each device runs one http.Server bound to dev.Ports.SOAP
// and serves POST /onvif/device_service, POST /onvif/media_service and an
// GET /onvif/snapshot (HTTP Basic when device auth is configured). Responses
// are hand-written XML templates matching the ONVIF WSDL element definitions;
// every dynamic value is XML-escaped. Method coverage follows docs/02 sections
// 4 and 5.
package onvif

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Aiaid/onvif-proxy/internal/config"
	"github.com/Aiaid/onvif-proxy/internal/soap"
)

const (
	// clockSkew is the tolerance applied to the WSSE Created timestamp.
	clockSkew = 5 * time.Minute

	maxSOAPBodyBytes  = 1 << 20 // 1 MiB
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	idleTimeout       = 60 * time.Second
	maxHeaderBytes    = 64 << 10 // 64 KiB
	maxNonceCache     = 4096
)

// Options configures a Server. It carries values injected by main that cannot
// be derived from the device config alone.
type Options struct {
	// AdvertiseIP is the fallback host used to build URIs when a request
	// carries no Host header, and the address reported for eth0.
	AdvertiseIP string
	// Version is the firmware version shown by GetDeviceInformation.
	Version string
	// SnapshotFunc produces a JPEG for GET /onvif/snapshot. It is injected by
	// main (passthrough / ffmpeg / cache are composed there).
	SnapshotFunc func(ctx context.Context, streamName string) (data []byte, contentType string, err error)
}

// Server serves the ONVIF services for one device.
type Server struct {
	dev        *config.Device
	opts       Options
	mux        *http.ServeMux
	nonceMu    sync.Mutex
	usedNonces map[[32]byte]time.Time
}

// NewServer builds a Server and its route table for dev.
func NewServer(dev *config.Device, opts Options) *Server {
	s := &Server{
		dev:        dev,
		opts:       opts,
		mux:        http.NewServeMux(),
		usedNonces: make(map[[32]byte]time.Time),
	}
	s.mux.HandleFunc("POST /onvif/device_service", s.handleDevice)
	s.mux.HandleFunc("POST /onvif/media_service", s.handleMedia)
	s.mux.HandleFunc("GET /onvif/snapshot", s.handleSnapshot)
	return s
}

// Run listens on dev.Ports.SOAP and serves until ctx is cancelled, then shuts
// down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", s.dev.Ports.SOAP))
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves on an already-bound listener. It lets callers reserve and
// validate a port before replacing a running configuration generation.
// Serve owns ln and closes it during shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := s.httpServer()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// ---- request helpers ------------------------------------------------------

// parse reads and decodes a size-limited SOAP request body.
func parse(w http.ResponseWriter, r *http.Request) (*soap.Request, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSOAPBodyBytes))
	if err != nil {
		return nil, err
	}
	return soap.ParseRequest(body)
}

// authorized reports whether the request may invoke action. When the device
// has no configured auth every request is allowed. GetSystemDateAndTime is
// always allowed so clients can synchronise their clock before computing a
// PasswordDigest.
func (s *Server) authorized(req *soap.Request, action string) bool {
	if s.dev.Auth == nil || action == "GetSystemDateAndTime" {
		return true
	}
	if req.Security == nil {
		return false
	}
	if !req.Security.Verify(s.dev.Auth.Username, s.dev.Auth.Password, clockSkew) {
		return false
	}
	if !req.Security.IsDigest() {
		return true
	}
	return s.acceptNonce(req.Security.Username, req.Security.Nonce, req.Security.Created)
}

// acceptNonce rejects replay of a successfully verified digest during the
// WSSE clock-skew window. The cache is bounded in time and can only be filled
// by a caller that already knows the configured password.
func (s *Server) acceptNonce(username, nonce, created string) bool {
	key := sha256.Sum256([]byte(username + "\x00" + nonce + "\x00" + created))
	now := time.Now()

	s.nonceMu.Lock()
	defer s.nonceMu.Unlock()
	for k, seen := range s.usedNonces {
		if now.Sub(seen) > 2*clockSkew {
			delete(s.usedNonces, k)
		}
	}
	if _, exists := s.usedNonces[key]; exists {
		return false
	}
	if len(s.usedNonces) >= maxNonceCache {
		var oldestKey [32]byte
		var oldestTime time.Time
		for k, seen := range s.usedNonces {
			if oldestTime.IsZero() || seen.Before(oldestTime) {
				oldestKey, oldestTime = k, seen
			}
		}
		delete(s.usedNonces, oldestKey)
	}
	s.usedNonces[key] = now
	return true
}

// write renders an ONVIF response body inside a SOAP envelope.
func write(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", soap.ContentType)
	w.WriteHeader(http.StatusOK)
	w.Write(soap.Envelope(body))
}

// hostPort returns the Host header (including any port) or the advertise IP as
// a fallback. Used for HTTP XAddrs and the snapshot URI.
func (s *Server) hostPort(r *http.Request) string {
	if r.Host != "" {
		return r.Host
	}
	return s.opts.AdvertiseIP
}

// hostname returns just the host portion of the request (no port), falling
// back to the advertise IP. Used to build RTSP URIs.
func (s *Server) hostname(r *http.Request) string {
	h := r.Host
	if h == "" {
		return s.opts.AdvertiseIP
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	if h == "" {
		return s.opts.AdvertiseIP
	}
	return h
}

// advertiseIP returns the address reported for the eth0 interface.
func (s *Server) advertiseIP(r *http.Request) string {
	if s.opts.AdvertiseIP != "" {
		return s.opts.AdvertiseIP
	}
	return s.hostname(r)
}

// ---- snapshot -------------------------------------------------------------

// handleSnapshot serves GET /onvif/snapshot?token=<profileToken>. When ONVIF
// auth is configured it requires the same credentials via HTTP Basic.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.dev.Auth != nil && !s.snapshotAuthorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="ONVIF Snapshot"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := r.URL.Query().Get("token")
	stream := s.dev.StreamByProfileToken(token)
	if stream == nil {
		http.Error(w, "unknown profile token", http.StatusNotFound)
		return
	}
	if s.opts.SnapshotFunc == nil {
		http.Error(w, "snapshot not available", http.StatusServiceUnavailable)
		return
	}
	data, contentType, err := s.opts.SnapshotFunc(r.Context(), stream.Name)
	if err != nil {
		http.Error(w, "snapshot failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if contentType == "" {
		contentType = "image/jpeg"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *Server) snapshotAuthorized(r *http.Request) bool {
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	gotUser, wantUser := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(s.dev.Auth.Username))
	gotPass, wantPass := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(s.dev.Auth.Password))
	userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
	passOK := subtle.ConstantTimeCompare(gotPass[:], wantPass[:])
	return userOK&passOK == 1
}

// ---- info defaults --------------------------------------------------------

func (s *Server) manufacturer() string {
	if s.dev.Info.Manufacturer != "" {
		return s.dev.Info.Manufacturer
	}
	return "ONVIF-Proxy"
}

func (s *Server) model() string {
	if s.dev.Info.Model != "" {
		return s.dev.Info.Model
	}
	return "Virtual-Camera"
}

func (s *Server) firmware() string {
	if s.dev.Info.Firmware != "" {
		return s.dev.Info.Firmware
	}
	if s.opts.Version != "" {
		return s.opts.Version
	}
	return "1.0.0"
}

// slug turns the device name into a hostname-safe label.
func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "camera"
	}
	return out
}
