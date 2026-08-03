package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aiaid/onvif-proxy/internal/config"
	"github.com/Aiaid/onvif-proxy/internal/discovery"
)

const testYAML = `
web:
  port: 8080
devices:
  - name: cam1
    uuid: 11111111-2222-3333-4444-555555555555
    ports:
      soap: 8081
      rtsp: 8554
    streams:
      - name: main
        rtsp: rtsp://user:pass@192.168.1.50:554/ch1/main
        width: 1920
        height: 1080
        framerate: 25
        bitrate: 4096
`

// mockBackend is a minimal Backend for HTTP-layer tests.
type mockBackend struct {
	yaml       []byte
	applyErr   error
	applied    []byte
	appliedDry bool
	devices    []DeviceRuntime
}

type dynamicWebBackend struct {
	*mockBackend
	web config.WebConfig
}

func (b *dynamicWebBackend) WebConfig() config.WebConfig { return b.web }

func (m *mockBackend) ConfigYAML() ([]byte, error) { return m.yaml, nil }
func (m *mockBackend) ApplyConfig(raw []byte, dryRun bool) error {
	m.applied = raw
	m.appliedDry = dryRun
	return m.applyErr
}
func (m *mockBackend) Devices() []DeviceRuntime { return m.devices }
func (m *mockBackend) Snapshot(_ context.Context, _ *config.Device, _ string) ([]byte, string, error) {
	return []byte{0xff, 0xd8}, "image/jpeg", nil
}
func (m *mockBackend) DiscoveryLog() []discovery.LogEntry { return nil }
func (m *mockBackend) Status() Status {
	return Status{Version: "test", AdvertiseIP: "10.0.0.1", FFmpeg: true}
}

func newTestBackend(t *testing.T) *mockBackend {
	t.Helper()
	cfg, err := config.Parse([]byte(testYAML))
	if err != nil {
		t.Fatalf("parse test yaml: %v", err)
	}
	devs := make([]DeviceRuntime, 0, len(cfg.Devices))
	for _, d := range cfg.Devices {
		devs = append(devs, DeviceRuntime{Device: d, Running: true})
	}
	return &mockBackend{yaml: []byte(testYAML), devices: devs}
}

func do(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func TestGetConfig(t *testing.T) {
	s := New(config.WebConfig{Port: 8080}, newTestBackend(t))
	rec := do(s, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cam1") {
		t.Fatalf("body missing config: %q", rec.Body.String())
	}
}

func TestPutConfigDryRun(t *testing.T) {
	b := newTestBackend(t)
	s := New(config.WebConfig{Port: 8080}, b)
	req := httptest.NewRequest(http.MethodPut, "/api/config?dry_run=1", strings.NewReader("x: 1"))
	rec := do(s, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !b.appliedDry {
		t.Fatalf("expected dry-run flag to be set")
	}
}

func TestOversizedRequestBodiesReturn413(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		prefix string
		limit  int
	}{
		{"config", http.MethodPut, "/api/config", testYAML, maxConfigBodyBytes},
		{"add device", http.MethodPost, "/api/devices", addDeviceBody, maxDeviceBodyBytes},
		{"rtsp probe", http.MethodPost, "/api/test/rtsp", `{"url":"rtsp://192.0.2.1/main"}`, maxProbeBodyBytes},
		{"stream info", http.MethodPost, "/api/test/streaminfo", `{"url":"rtsp://192.0.2.1/main"}`, maxProbeBodyBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestBackend(t)
			s := New(config.WebConfig{Port: 8080}, b)
			body := tt.prefix + strings.Repeat(" ", tt.limit-len(tt.prefix)+1)
			rec := do(s, httptest.NewRequest(tt.method, tt.path, strings.NewReader(body)))
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
			}
			if b.applied != nil {
				t.Fatal("oversized body reached ApplyConfig")
			}
		})
	}
}

func TestDevices(t *testing.T) {
	s := New(config.WebConfig{Port: 8080}, newTestBackend(t))
	rec := do(s, httptest.NewRequest(http.MethodGet, "/api/devices", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []deviceView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Name != "cam1" {
		t.Fatalf("unexpected devices: %+v", got)
	}
	st := got[0].Endpoints.Streams
	if len(st) != 1 || st[0].ProfileToken != "profile_main" {
		t.Fatalf("unexpected streams: %+v", st)
	}
	want := "rtsp://10.0.0.1:8554/ch1/main"
	if st[0].RTSPURI != want {
		t.Fatalf("rtsp_uri = %q, want %q", st[0].RTSPURI, want)
	}
	if st[0].RTSP != "rtsp://192.168.1.50:554/ch1/main" || !st[0].HasCredentials {
		t.Fatalf("upstream credentials were not safely represented: %+v", st[0])
	}
	if st[0].Codec != "h264" {
		t.Fatalf("default codec = %q, want h264", st[0].Codec)
	}
	if strings.Contains(rec.Body.String(), "user:pass") {
		t.Fatalf("GET /api/devices leaked RTSP credentials: %s", rec.Body.String())
	}
}

func TestServesIndexShell(t *testing.T) {
	s := New(config.WebConfig{Port: 8080}, newTestBackend(t))
	rec := do(s, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="app"`) || !strings.Contains(body, "/dist/app.js") {
		t.Fatalf("index is not the SPA shell:\n%s", body)
	}
}

func TestServesBundle(t *testing.T) {
	s := New(config.WebConfig{Port: 8080}, newTestBackend(t))
	rec := do(s, httptest.NewRequest(http.MethodGet, "/dist/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dist/app.js status = %d (is the bundle built and committed?)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("dist/app.js content-type = %q", ct)
	}
}

func TestBasicAuthChallenge(t *testing.T) {
	s := New(config.WebConfig{Port: 8080, Username: "admin", Password: "secret"}, newTestBackend(t))

	rec := do(s, httptest.NewRequest(http.MethodGet, "/api/devices", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("missing WWW-Authenticate header")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	req.SetBasicAuth("admin", "secret")
	if rec := do(s, req); rec.Code != http.StatusOK {
		t.Fatalf("authed status = %d", rec.Code)
	}
}

func TestBasicAuthCredentialsReloadDynamically(t *testing.T) {
	b := &dynamicWebBackend{
		mockBackend: newTestBackend(t),
		web:         config.WebConfig{Port: 8080, Username: "admin", Password: "old"},
	}
	s := New(b.web, b)

	oldReq := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	oldReq.SetBasicAuth("admin", "old")
	if rec := do(s, oldReq); rec.Code != http.StatusOK {
		t.Fatalf("old credentials status = %d", rec.Code)
	}

	b.web.Username, b.web.Password = "operator", "new"
	staleReq := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	staleReq.SetBasicAuth("admin", "old")
	if rec := do(s, staleReq); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale credentials status = %d, want 401", rec.Code)
	}
	newReq := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	newReq.SetBasicAuth("operator", "new")
	if rec := do(s, newReq); rec.Code != http.StatusOK {
		t.Fatalf("reloaded credentials status = %d", rec.Code)
	}
}

func TestTestRTSPRejectsNonRTSPScheme(t *testing.T) {
	s := New(config.WebConfig{Port: 8080}, newTestBackend(t))
	req := httptest.NewRequest(http.MethodPost, "/api/test/rtsp",
		strings.NewReader(`{"url":"http://example.com/x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := do(s, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestTestStreamInfoRejectsNonRTSPScheme(t *testing.T) {
	s := New(config.WebConfig{Port: 8080}, newTestBackend(t))
	req := httptest.NewRequest(http.MethodPost, "/api/test/streaminfo",
		strings.NewReader(`{"url":"http://example.com/x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := do(s, req)
	// When ffmpeg is unavailable the handler short-circuits with 501 before the
	// scheme check; both outcomes reject the request, which is what we assert.
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 400 or 501", rec.Code)
	}
}

const addDeviceBody = `{
  "name": "cam2",
  "soap_port": 8082,
  "rtsp_port": 8555,
  "auth": {"username": "admin", "password": "secret"},
  "streams": [
    {"name": "main", "rtsp": "rtsp://user:pass@192.168.1.60:554/ch1/main",
     "width": 1280, "height": 720, "framerate": 20, "bitrate": 2048}
  ],
  "snapshot": {"url": "", "stream": "", "cache_seconds": 0}
}`

func TestAddDeviceSuccess(t *testing.T) {
	b := newTestBackend(t)
	s := New(config.WebConfig{Port: 8080}, b)
	req := httptest.NewRequest(http.MethodPost, "/api/devices", strings.NewReader(addDeviceBody))
	req.Header.Set("Content-Type", "application/json")
	rec := do(s, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if b.applied == nil {
		t.Fatal("ApplyConfig was not called")
	}
	if b.appliedDry {
		t.Fatal("device add must not be a dry run")
	}
	applied := string(b.applied)
	// The new device is present and the original device is preserved.
	if !strings.Contains(applied, "cam2") {
		t.Fatalf("applied yaml missing new device:\n%s", applied)
	}
	if !strings.Contains(applied, "cam1") {
		t.Fatalf("applied yaml dropped existing device:\n%s", applied)
	}
	// The applied YAML must itself re-parse cleanly (2-space indent, valid shape).
	if _, err := config.Parse(b.applied); err != nil {
		t.Fatalf("applied yaml does not re-parse: %v\n%s", err, applied)
	}
}

func TestAddDevicePortConflict(t *testing.T) {
	b := newTestBackend(t)
	b.applyErr = &config.ValidationError{Problems: []string{"port 8081 used by both web and devices"}}
	s := New(config.WebConfig{Port: 8080}, b)
	req := httptest.NewRequest(http.MethodPost, "/api/devices", strings.NewReader(addDeviceBody))
	req.Header.Set("Content-Type", "application/json")
	rec := do(s, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "port 8081") {
		t.Fatalf("validation detail not surfaced: %s", rec.Body.String())
	}
}

const editDeviceBody = `{
  "name": "cam1-renamed",
  "soap_port": 8081,
  "rtsp_port": 8554,
  "streams": [
    {"name": "main", "rtsp": "rtsp://192.168.1.99:554/ch9/main", "codec": "h264",
     "width": 2560, "height": 1440, "framerate": 30, "bitrate": 6144},
    {"name": "sub", "rtsp": "rtsp://192.168.1.100:554/ch9/sub", "codec": "jpeg",
     "width": 640, "height": 360, "framerate": 15, "bitrate": 512,
     "proxy_port": 8555}
  ]
}`

func TestEditDeviceSuccess(t *testing.T) {
	b := newTestBackend(t)
	s := New(config.WebConfig{Port: 8080}, b)
	req := httptest.NewRequest(http.MethodPut,
		"/api/devices/11111111-2222-3333-4444-555555555555", strings.NewReader(editDeviceBody))
	req.Header.Set("Content-Type", "application/json")
	rec := do(s, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if b.applied == nil {
		t.Fatal("ApplyConfig was not called")
	}
	if b.appliedDry {
		t.Fatal("device edit must not be a dry run")
	}
	applied := string(b.applied)
	// The edited values replaced the originals.
	if !strings.Contains(applied, "cam1-renamed") {
		t.Fatalf("applied yaml missing new name:\n%s", applied)
	}
	if !strings.Contains(applied, "192.168.1.99") || !strings.Contains(applied, "2560") {
		t.Fatalf("applied yaml missing new stream values:\n%s", applied)
	}
	if !strings.Contains(applied, "user:pass@192.168.1.99") {
		t.Fatalf("applied yaml dropped retained RTSP credentials:\n%s", applied)
	}
	if !strings.Contains(applied, "proxy_port: 8555") {
		t.Fatalf("applied yaml dropped per-stream proxy_port:\n%s", applied)
	}
	if !strings.Contains(applied, "codec: jpeg") {
		t.Fatalf("applied yaml dropped per-stream codec:\n%s", applied)
	}
	// The identity uuid is preserved (the form never carried it).
	if !strings.Contains(applied, "11111111-2222-3333-4444-555555555555") {
		t.Fatalf("applied yaml dropped original uuid:\n%s", applied)
	}
	// The old name must be gone (replaced in place, not appended).
	if strings.Contains(applied, "name: cam1\n") {
		t.Fatalf("original device not replaced:\n%s", applied)
	}
	if _, err := config.Parse(b.applied); err != nil {
		t.Fatalf("applied yaml does not re-parse: %v\n%s", err, applied)
	}
}

func TestPreserveRTSPCredentialsMatchesStreamNameAfterReorder(t *testing.T) {
	original := &config.Device{Streams: []*config.Stream{
		{Name: "main", RTSP: "rtsp://main:secret@192.0.2.10/main"},
		{Name: "sub", RTSP: "rtsp://sub:secret@192.0.2.11/sub"},
	}}
	edited := &config.Device{Streams: []*config.Stream{
		{Name: "new", RTSP: "rtsp://192.0.2.12/new"},
		{Name: "sub", RTSP: "rtsp://192.0.2.21/sub"},
		{Name: "main", RTSP: "rtsp://192.0.2.20/main"},
	}}

	preserveRTSPCredentials(edited, original)

	if strings.Contains(edited.Streams[0].RTSP, "@") {
		t.Fatalf("new stream inherited unrelated credentials: %q", edited.Streams[0].RTSP)
	}
	if !strings.Contains(edited.Streams[1].RTSP, "sub:secret@") {
		t.Fatalf("reordered sub stream lost its credentials: %q", edited.Streams[1].RTSP)
	}
	if !strings.Contains(edited.Streams[2].RTSP, "main:secret@") {
		t.Fatalf("reordered main stream lost its credentials: %q", edited.Streams[2].RTSP)
	}
}

func TestEditDeviceUnknownUUID(t *testing.T) {
	b := newTestBackend(t)
	s := New(config.WebConfig{Port: 8080}, b)
	req := httptest.NewRequest(http.MethodPut,
		"/api/devices/00000000-0000-0000-0000-000000000000", strings.NewReader(editDeviceBody))
	req.Header.Set("Content-Type", "application/json")
	rec := do(s, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 body=%s", rec.Code, rec.Body.String())
	}
	if b.applied != nil {
		t.Fatal("ApplyConfig must not be called for unknown uuid")
	}
}

func TestEditDeviceValidationError(t *testing.T) {
	b := newTestBackend(t)
	b.applyErr = &config.ValidationError{Problems: []string{"port 8081 used by both web and devices"}}
	s := New(config.WebConfig{Port: 8080}, b)
	req := httptest.NewRequest(http.MethodPut,
		"/api/devices/11111111-2222-3333-4444-555555555555", strings.NewReader(editDeviceBody))
	req.Header.Set("Content-Type", "application/json")
	rec := do(s, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "port 8081") {
		t.Fatalf("validation detail not surfaced: %s", rec.Body.String())
	}
}

const authYAML = `
web:
  port: 8080
devices:
  - name: cam1
    uuid: 11111111-2222-3333-4444-555555555555
    ports:
      soap: 8081
      rtsp: 8554
    auth:
      username: onvifuser
      password: onvifsecret
    streams:
      - name: main
        rtsp: rtsp://user:pass@192.168.1.50:554/ch1/main
        width: 1920
        height: 1080
        framerate: 25
        bitrate: 4096
`

// editKeepAuthBody keeps the username but sends a blank password, which the
// edit form does when the operator leaves the password field untouched.
const editKeepAuthBody = `{
  "name": "cam1",
  "soap_port": 8081,
  "rtsp_port": 8554,
  "auth": {"username": "onvifuser", "password": ""},
  "streams": [
    {"name": "main", "rtsp": "rtsp://user:pass@192.168.1.50:554/ch1/main",
     "width": 1920, "height": 1080, "framerate": 25, "bitrate": 4096}
  ]
}`

func TestEditDevicePreservesAuthPassword(t *testing.T) {
	b := &mockBackend{yaml: []byte(authYAML)}
	cfg, err := config.Parse([]byte(authYAML))
	if err != nil {
		t.Fatalf("parse auth yaml: %v", err)
	}
	for _, d := range cfg.Devices {
		b.devices = append(b.devices, DeviceRuntime{Device: d, Running: true})
	}
	s := New(config.WebConfig{Port: 8080}, b)
	req := httptest.NewRequest(http.MethodPut,
		"/api/devices/11111111-2222-3333-4444-555555555555", strings.NewReader(editKeepAuthBody))
	req.Header.Set("Content-Type", "application/json")
	rec := do(s, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	// The blank password must not wipe the original credential.
	if !strings.Contains(string(b.applied), "onvifsecret") {
		t.Fatalf("original auth password was dropped:\n%s", b.applied)
	}
}

func TestDeleteDeviceUnknownUUID(t *testing.T) {
	b := newTestBackend(t)
	s := New(config.WebConfig{Port: 8080}, b)
	req := httptest.NewRequest(http.MethodDelete, "/api/devices/00000000-0000-0000-0000-000000000000", nil)
	rec := do(s, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if b.applied != nil {
		t.Fatal("ApplyConfig must not be called for unknown uuid")
	}
}

func TestDeleteDeviceSuccess(t *testing.T) {
	b := newTestBackend(t)
	s := New(config.WebConfig{Port: 8080}, b)
	req := httptest.NewRequest(http.MethodDelete, "/api/devices/11111111-2222-3333-4444-555555555555", nil)
	rec := do(s, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if b.applied == nil {
		t.Fatal("ApplyConfig was not called")
	}
	if strings.Contains(string(b.applied), "cam1") {
		t.Fatalf("deleted device still present:\n%s", b.applied)
	}
}
