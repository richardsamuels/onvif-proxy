package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFilesArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteDefault(path); err != nil {
		t.Fatalf("WriteDefault() error = %v", err)
	}
	assertMode := func(want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat() error = %v", err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("config mode = %04o, want %04o", got, want)
		}
	}
	assertMode(0o600)

	cfg, err := Parse([]byte(validConfigYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	assertMode(0o600)
}

const validConfigYAML = `
server:
  advertise_ip: 192.0.2.10
web:
  port: 8080
devices:
  - name: camera
    uuid: 11111111-2222-4333-8444-555555555555
    mac: 02:00:00:00:00:01
    serial: camera-1
    ports:
      soap: 8081
      rtsp: 8554
    streams:
      - name: main
        rtsp: rtsp://user:pass@192.0.2.20/main
        width: 1920
        height: 1080
        framerate: 25
        bitrate: 4096
`

func TestParseRejectsNullEntries(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		problem string
	}{
		{
			name:    "null device",
			yaml:    "web:\n  port: 8080\ndevices: [null]\n",
			problem: "device must not be null",
		},
		{
			name: "null stream",
			yaml: strings.Replace(validConfigYAML,
				"      - name: main\n", "      - null\n      - name: main\n", 1),
			problem: "stream must not be null",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.problem) {
				t.Fatalf("Parse() error = %v, want problem %q", err, tt.problem)
			}
		})
	}
}

func TestParseRejectsAdditionalYAMLDocuments(t *testing.T) {
	for _, suffix := range []string{
		"---\n{}\n",
		"---\n# an empty trailing document\n",
	} {
		_, err := Parse([]byte(validConfigYAML + suffix))
		if err == nil || !strings.Contains(err.Error(), "multiple documents") {
			t.Fatalf("Parse() error = %v, want multiple-document rejection", err)
		}
	}
}

func TestValidationRejectsInvalidNetworkAndSnapshotFields(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		problem string
	}{
		{
			name:    "advertise hostname",
			yaml:    strings.Replace(validConfigYAML, "192.0.2.10", "camera.local", 1),
			problem: "advertise_ip",
		},
		{
			name:    "advertise IPv6",
			yaml:    strings.Replace(validConfigYAML, "192.0.2.10", "2001:db8::1", 1),
			problem: "valid IPv4",
		},
		{
			name:    "EUI-64 MAC",
			yaml:    strings.Replace(validConfigYAML, "02:00:00:00:00:01", "02:00:00:ff:fe:00:00:01", 1),
			problem: "invalid mac",
		},
		{
			name: "negative snapshot cache",
			yaml: strings.Replace(validConfigYAML, "    streams:\n",
				"    snapshot:\n      cache_seconds: -1\n    streams:\n", 1),
			problem: "cache_seconds must not be negative",
		},
		{
			name: "snapshot URL without host",
			yaml: strings.Replace(validConfigYAML, "    streams:\n",
				"    snapshot:\n      url: http:path\n    streams:\n", 1),
			problem: "valid http(s) URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.problem) {
				t.Fatalf("Parse() error = %v, want problem %q", err, tt.problem)
			}
		})
	}
}

func TestStreamCodecValidationAndDefault(t *testing.T) {
	cfg, err := Parse([]byte(validConfigYAML))
	if err != nil {
		t.Fatalf("Parse(default codec) error = %v", err)
	}
	if got := cfg.Devices[0].Streams[0].CodecName(); got != "h264" {
		t.Fatalf("default CodecName() = %q, want h264", got)
	}

	for _, codec := range []string{"h264", "jpeg", "mpeg4"} {
		t.Run(codec, func(t *testing.T) {
			data := strings.Replace(validConfigYAML, "        rtsp:", "        codec: "+codec+"\n        rtsp:", 1)
			cfg, err := Parse([]byte(data))
			if err != nil {
				t.Fatalf("Parse(%s) error = %v", codec, err)
			}
			if got := cfg.Devices[0].Streams[0].CodecName(); got != codec {
				t.Fatalf("CodecName() = %q, want %q", got, codec)
			}
		})
	}

	for _, codec := range []string{"h265", "hevc"} {
		t.Run("reject_"+codec, func(t *testing.T) {
			data := strings.Replace(validConfigYAML, "        rtsp:", "        codec: "+codec+"\n        rtsp:", 1)
			_, err := Parse([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "not supported by ONVIF Profile S") {
				t.Fatalf("Parse(%s) error = %v, want Profile S rejection", codec, err)
			}
		})
	}
}

func TestProxyPortCanBeSharedByStreamsOnSameUpstream(t *testing.T) {
	data := strings.Replace(validConfigYAML, "        bitrate: 4096\n", `        bitrate: 4096
      - name: sub
        rtsp: rtsp://192.0.2.21/sub
        width: 640
        height: 360
        framerate: 15
        bitrate: 512
        proxy_port: 8555
      - name: mobile
        rtsp: rtsp://192.0.2.21/mobile
        width: 320
        height: 180
        framerate: 10
        bitrate: 256
        proxy_port: 8555
`, 1)

	cfg, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	got := cfg.Devices[0].ProxyBindings()
	if len(got) != 2 {
		t.Fatalf("ProxyBindings() = %+v, want two deduplicated bindings", got)
	}
	if got[1].ListenPort != 8555 || got[1].Target != "192.0.2.21:554" {
		t.Fatalf("secondary binding = %+v", got[1])
	}
}

func TestProxyPortCannotMixUpstreams(t *testing.T) {
	data := strings.Replace(validConfigYAML, "        bitrate: 4096\n", `        bitrate: 4096
      - name: sub
        rtsp: rtsp://192.0.2.21/sub
        width: 640
        height: 360
        framerate: 15
        bitrate: 512
        proxy_port: 8555
      - name: mobile
        rtsp: rtsp://192.0.2.22/mobile
        width: 320
        height: 180
        framerate: 10
        bitrate: 256
        proxy_port: 8555
`, 1)
	_, err := Parse([]byte(data))
	if err == nil || !strings.Contains(err.Error(), "cannot also point") {
		t.Fatalf("Parse() error = %v, want mixed-upstream rejection", err)
	}
}

func TestSameUpstreamMustReuseProxyPort(t *testing.T) {
	data := strings.Replace(validConfigYAML, "        bitrate: 4096\n", `        bitrate: 4096
      - name: sub
        rtsp: rtsp://192.0.2.21/sub
        width: 640
        height: 360
        framerate: 15
        bitrate: 512
        proxy_port: 8555
      - name: mobile
        rtsp: rtsp://192.0.2.21/mobile
        width: 320
        height: 180
        framerate: 10
        bitrate: 256
        proxy_port: 8556
`, 1)
	_, err := Parse([]byte(data))
	if err == nil || !strings.Contains(err.Error(), "already uses proxy_port 8555") {
		t.Fatalf("Parse() error = %v, want inconsistent-port rejection", err)
	}
}

func TestPreparePreservesOriginalBytesOrAddsIdentities(t *testing.T) {
	cfg, unchanged, err := Prepare([]byte(validConfigYAML))
	if err != nil {
		t.Fatalf("Prepare(valid) error = %v", err)
	}
	if cfg.Devices[0].UUID == "" || !bytes.Equal(unchanged, []byte(validConfigYAML)) {
		t.Fatal("Prepare changed a config that already had identities")
	}

	missing := strings.Replace(validConfigYAML,
		"    uuid: 11111111-2222-4333-8444-555555555555\n    mac: 02:00:00:00:00:01\n    serial: camera-1\n",
		"", 1)
	generated, prepared, err := Prepare([]byte(missing))
	if err != nil {
		t.Fatalf("Prepare(missing identities) error = %v", err)
	}
	dev := generated.Devices[0]
	if dev.UUID == "" || dev.MAC == "" || dev.Serial == "" {
		t.Fatalf("identities not generated: %+v", dev)
	}
	if bytes.Equal(prepared, []byte(missing)) {
		t.Fatal("prepared YAML was not updated with generated identities")
	}
	if _, err := Parse(prepared); err != nil {
		t.Fatalf("prepared YAML does not parse: %v", err)
	}
}

func TestAlarmConfig(t *testing.T) {
	cfg, err := Parse([]byte(validConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Alarm.Enabled || cfg.Alarm.ListenPort() != DefaultAlarmPort || cfg.Alarm.Hold() != DefaultAlarmHoldSeconds {
		t.Fatalf("defaults: %+v port=%d hold=%d", cfg.Alarm, cfg.Alarm.ListenPort(), cfg.Alarm.Hold())
	}
	out, err := marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "alarm") {
		t.Fatalf("zero alarm section should be omitted:\n%s", out)
	}

	cfg, err = Parse([]byte(validConfigYAML + "alarm:\n  enabled: true\n  port: 9000\n  hold_seconds: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Alarm.Enabled || cfg.Alarm.ListenPort() != 9000 || cfg.Alarm.Hold() != 0 {
		t.Fatalf("explicit: %+v", cfg.Alarm)
	}

	for name, extra := range map[string]string{
		"port conflict": "alarm:\n  enabled: true\n  port: 8081\n",
		"negative hold": "alarm:\n  hold_seconds: -1\n",
		"bad port":      "alarm:\n  enabled: true\n  port: 70000\n",
	} {
		if _, err := Parse([]byte(validConfigYAML + extra)); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	// A disabled receiver does not claim its port.
	if _, err := Parse([]byte(validConfigYAML + "alarm:\n  enabled: false\n  port: 8081\n")); err != nil {
		t.Errorf("disabled alarm port should not conflict: %v", err)
	}
}
