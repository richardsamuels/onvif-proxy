package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aiaid/onvif-proxy/internal/config"
	"gopkg.in/yaml.v3"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freePorts(t *testing.T, n int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, opened := range listeners {
				opened.Close()
			}
			t.Fatal(err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range listeners {
		ln.Close()
	}
	return ports
}

func managerTestConfig(t *testing.T, soapPort, rtspPort int) (*config.Config, []byte) {
	t.Helper()
	raw := []byte(fmt.Sprintf(`
server:
  advertise_ip: "127.0.0.1"
  discovery: false
web:
  enabled: false
devices:
  - name: test
    ports: {soap: %d, rtsp: %d}
    streams:
      - name: main
        rtsp: rtsp://127.0.0.1:554/main
        width: 1920
        height: 1080
        framerate: 25
        bitrate: 2048
`, soapPort, rtspPort))
	cfg, persisted, err := config.Prepare(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, persisted
}

func TestManagerStartReportsBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	cfg, raw := managerTestConfig(t, occupied.Addr().(*net.TCPAddr).Port, freePort(t))
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m := newManager(path, cfg)
	err = m.start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SOAP port") {
		t.Fatalf("start error = %v, want synchronous SOAP bind failure", err)
	}
}

func TestApplyConfigRestoresPreviousRuntimeOnBindFailure(t *testing.T) {
	ports := freePorts(t, 2)
	oldSOAP, oldRTSP := ports[0], ports[1]
	cfg, initialRaw := managerTestConfig(t, oldSOAP, oldRTSP)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, initialRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newManager(path, cfg)
	if err := m.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.stopAll()

	occupied, err := net.Listen("tcp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	candidate := *cfg
	candidate.Devices = append([]*config.Device(nil), cfg.Devices...)
	devCopy := *cfg.Devices[0]
	devCopy.Ports.SOAP = occupied.Addr().(*net.TCPAddr).Port
	candidate.Devices[0] = &devCopy
	candidateRaw, err := yaml.Marshal(&candidate)
	if err != nil {
		t.Fatal(err)
	}

	err = m.ApplyConfig(candidateRaw, false)
	if err == nil || !strings.Contains(err.Error(), "previous runtime restored") {
		t.Fatalf("ApplyConfig error = %v, want rollback error", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, initialRaw) {
		t.Fatal("failed apply changed the on-disk config")
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", oldSOAP), time.Second)
	if err != nil {
		t.Fatalf("previous SOAP listener was not restored: %v", err)
	}
	conn.Close()
	devices := m.Devices()
	if len(devices) != 1 || !devices[0].Running || devices[0].Error != "" {
		t.Fatalf("runtime after rollback = %+v", devices)
	}
}

func TestApplyConfigAtomicallyPersistsGeneratedIdentity(t *testing.T) {
	ports := freePorts(t, 2)
	soapPort, rtspPort := ports[0], ports[1]
	cfg, initialRaw := managerTestConfig(t, soapPort, rtspPort)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, initialRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newManager(path, cfg)
	if err := m.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.stopAll()

	// Clearing identity simulates a newly added device. Prepare must generate it
	// before the candidate runtime starts and the atomic writer must persist it.
	candidate := *cfg
	candidate.Devices = append([]*config.Device(nil), cfg.Devices...)
	devCopy := *cfg.Devices[0]
	devCopy.Name = "renamed"
	devCopy.UUID, devCopy.MAC, devCopy.Serial = "", "", ""
	candidate.Devices[0] = &devCopy
	raw, err := yaml.Marshal(&candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyConfig(raw, false); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 600", got)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Devices[0].UUID == "" || loaded.Devices[0].MAC == "" || loaded.Devices[0].Serial == "" {
		t.Fatal("generated identity was not persisted")
	}
}

func TestApplyConfigRejectsLiveWebChangeWithoutWriting(t *testing.T) {
	ports := freePorts(t, 2)
	cfg, initialRaw := managerTestConfig(t, ports[0], ports[1])
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, initialRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newManager(path, cfg)
	if err := m.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.stopAll()

	candidate := *cfg
	enabled := true
	candidate.Web.Enabled = &enabled
	candidate.Web.Port = freePort(t)
	raw, err := yaml.Marshal(&candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyConfig(raw, false); err == nil || !strings.Contains(err.Error(), "require a process restart") {
		t.Fatalf("ApplyConfig error = %v", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, initialRaw) {
		t.Fatal("rejected web change modified config")
	}
}

func TestApplyConfigReloadsWebCredentials(t *testing.T) {
	ports := freePorts(t, 2)
	cfg, initialRaw := managerTestConfig(t, ports[0], ports[1])
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, initialRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newManager(path, cfg)
	if err := m.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.stopAll()

	candidate := *cfg
	candidate.Web.Username = "operator"
	candidate.Web.Password = "rotated-secret"
	raw, err := yaml.Marshal(&candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyConfig(raw, false); err != nil {
		t.Fatal(err)
	}
	got := m.WebConfig()
	if got.Username != "operator" || got.Password != "rotated-secret" {
		t.Fatal("effective web credentials were not reloaded")
	}
}
