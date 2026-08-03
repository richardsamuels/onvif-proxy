// Package config defines the YAML configuration model shared by every other
// package: loading, strict parsing, validation, default generation (UUID/MAC)
// and atomic write-back.
package config

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  ServerConfig `yaml:"server"`
	Web     WebConfig    `yaml:"web"`
	Devices []*Device    `yaml:"devices"`
}

type ServerConfig struct {
	// AdvertiseIP is written into WS-Discovery XAddrs and used as a fallback
	// for URIs when no Host header is available. Empty = auto-detect.
	AdvertiseIP string `yaml:"advertise_ip"`
	Discovery   *bool  `yaml:"discovery"` // nil = true
}

func (s ServerConfig) DiscoveryEnabled() bool { return s.Discovery == nil || *s.Discovery }

type WebConfig struct {
	Enabled  *bool  `yaml:"enabled"` // nil = true
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

func (w WebConfig) IsEnabled() bool { return w.Enabled == nil || *w.Enabled }

type Device struct {
	Name     string    `yaml:"name"`
	UUID     string    `yaml:"uuid"`
	MAC      string    `yaml:"mac"`
	Serial   string    `yaml:"serial"`
	Ports    Ports     `yaml:"ports"`
	Info     Info      `yaml:"info"`
	Auth     *Auth     `yaml:"auth,omitempty"`
	Streams  []*Stream `yaml:"streams"`
	Snapshot Snapshot  `yaml:"snapshot"`
}

type Ports struct {
	SOAP int `yaml:"soap"`
	RTSP int `yaml:"rtsp"`
}

type Info struct {
	Manufacturer string `yaml:"manufacturer"`
	Model        string `yaml:"model"`
	Firmware     string `yaml:"firmware"`
}

type Auth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Snapshot struct {
	URL          string `yaml:"url"`    // passthrough mode when set
	Stream       string `yaml:"stream"` // ffmpeg grab source, default streams[0]
	CacheSeconds int    `yaml:"cache_seconds"`
}

type Stream struct {
	Name string `yaml:"name"`
	RTSP string `yaml:"rtsp"`
	// Codec is the Profile S video encoding: h264 (default), jpeg, or mpeg4.
	Codec     string `yaml:"codec,omitempty"`
	Width     int    `yaml:"width"`
	Height    int    `yaml:"height"`
	Framerate int    `yaml:"framerate"`
	Bitrate   int    `yaml:"bitrate"`
	// ProxyPort must be set when this stream's upstream host:port differs from
	// the primary stream's; otherwise it shares Device.Ports.RTSP.
	ProxyPort int `yaml:"proxy_port,omitempty"`

	u *url.URL
}

// ---- Stream helpers -------------------------------------------------------

// TargetAddr returns the upstream "host:port" (default RTSP port 554).
func (s *Stream) TargetAddr() string {
	host, port := s.u.Hostname(), s.u.Port()
	if port == "" {
		port = "554"
	}
	return net.JoinHostPort(host, port)
}

// PathQuery returns the RTSP path plus query, e.g. "/ch1/main?token=x".
func (s *Stream) PathQuery() string {
	p := s.u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if s.u.RawQuery != "" {
		p += "?" + s.u.RawQuery
	}
	return p
}

// SourceURL returns the full upstream URL including credentials (for ffmpeg
// and the probe client, never exposed via ONVIF).
func (s *Stream) SourceURL() string { return s.RTSP }

// CodecName returns the canonical configured codec. Empty keeps older configs
// backward compatible by defaulting to H.264.
func (s *Stream) CodecName() string {
	if s.Codec == "" {
		return "h264"
	}
	return strings.ToLower(s.Codec)
}

func (s *Stream) ProfileToken() string { return "profile_" + s.Name }
func (s *Stream) EncoderToken() string { return "vec_" + s.Name }

// ---- Device helpers -------------------------------------------------------

func (d *Device) PrimaryStream() *Stream { return d.Streams[0] }

func (d *Device) StreamByName(name string) *Stream {
	for _, s := range d.Streams {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func (d *Device) StreamByProfileToken(token string) *Stream {
	for _, s := range d.Streams {
		if s.ProfileToken() == token {
			return s
		}
	}
	return nil
}

// ProxyPortFor returns the local RTSP proxy port serving the given stream.
func (d *Device) ProxyPortFor(s *Stream) int {
	if s.ProxyPort != 0 {
		return s.ProxyPort
	}
	return d.Ports.RTSP
}

// SnapshotStream returns the stream used for ffmpeg frame grabs.
func (d *Device) SnapshotStream() *Stream {
	if d.Snapshot.Stream != "" {
		if s := d.StreamByName(d.Snapshot.Stream); s != nil {
			return s
		}
	}
	return d.PrimaryStream()
}

func (d *Device) SnapshotCacheSeconds() int {
	if d.Snapshot.CacheSeconds > 0 {
		return d.Snapshot.CacheSeconds
	}
	return 10
}

// ProxyBinding maps a local listen port to an upstream target address.
type ProxyBinding struct {
	ListenPort int
	Target     string
}

// ProxyBindings returns the deduplicated set of TCP proxies this device needs.
func (d *Device) ProxyBindings() []ProxyBinding {
	seen := map[int]bool{}
	var out []ProxyBinding
	for _, s := range d.Streams {
		p := d.ProxyPortFor(s)
		if !seen[p] {
			seen[p] = true
			out = append(out, ProxyBinding{ListenPort: p, Target: s.TargetAddr()})
		}
	}
	return out
}

// ---- Load / Parse / Save --------------------------------------------------

// DefaultYAML is the starter config written when none exists yet.
const DefaultYAML = `# onvif-proxy configuration (auto-generated).
# Add virtual devices below or through the web UI config editor.
# Full field reference: docs/03-config.md

server:
  # IP advertised in WS-Discovery and stream URIs. Empty = auto-detect.
  advertise_ip: ""
  discovery: true

web:
  enabled: true
  port: 8080
  # HTTP Basic auth for the web UI. Empty = no auth.
  username: ""
  password: ""

devices: []
`

// WriteDefault creates a starter config at path. It never overwrites: the
// file is created with O_EXCL and an error is returned if it already exists.
func WriteDefault(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(DefaultYAML); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Load reads, validates and default-fills the config. Generated identity
// fields (uuid/mac/serial) are persisted back to the same file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if cfg.fillGenerated() {
		if err := Save(path, cfg); err != nil {
			return nil, fmt.Errorf("write back generated identities: %w", err)
		}
	}
	return cfg, nil
}

// Parse strictly decodes and validates YAML. It also fills non-persisted
// defaults (web port). Generated identities are NOT filled here; callers that
// need them use Load or fillGenerated.
func Parse(data []byte) (*Config, error) {
	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("yaml trailing document: %w", err)
		}
		return nil, fmt.Errorf("yaml: multiple documents are not allowed")
	}
	if cfg.Web.Port == 0 {
		cfg.Web.Port = 8080
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Prepare strictly parses data, fills generated device identities, and returns
// the exact YAML bytes that should be persisted. When no identities need to be
// generated, the original bytes are returned unchanged so comments and
// formatting survive a transactional apply.
func Prepare(data []byte) (*Config, []byte, error) {
	cfg, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.fillGenerated() {
		return cfg, data, nil
	}
	prepared, err := marshal(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal generated identities: %w", err)
	}
	return cfg, prepared, nil
}

// Save atomically writes the config. Note: comments in a hand-written file
// are not preserved (the struct is re-marshaled).
func Save(path string, cfg *Config) error {
	data, err := marshal(cfg)
	if err != nil {
		return err
	}
	return saveBytes(path, data)
}

func marshal(cfg *Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func saveBytes(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// fillGenerated fills uuid/mac/serial and reports whether anything changed.
func (c *Config) fillGenerated() bool {
	changed := false
	for _, d := range c.Devices {
		if d.UUID == "" {
			d.UUID = NewUUID()
			changed = true
		}
		if d.MAC == "" {
			d.MAC = NewMAC()
			changed = true
		}
		if d.Serial == "" {
			d.Serial = strings.ReplaceAll(d.UUID, "-", "")[:8]
			changed = true
		}
	}
	return changed
}

// ---- Validation ------------------------------------------------------------

var streamNameRe = regexp.MustCompile(`^[a-z0-9_]+$`)
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid config (%d problems):\n  - %s",
		len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

func (c *Config) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if c.Server.AdvertiseIP != "" {
		ip := net.ParseIP(c.Server.AdvertiseIP)
		if ip == nil || ip.To4() == nil {
			add("server.advertise_ip: must be a valid IPv4 address")
		}
	}

	ports := map[int]string{} // port -> owner description
	claim := func(port int, owner string) {
		if port <= 0 || port > 65535 {
			add("%s: port %d out of range", owner, port)
			return
		}
		if prev, dup := ports[port]; dup {
			add("port %d used by both %s and %s", port, prev, owner)
			return
		}
		ports[port] = owner
	}

	if c.Web.IsEnabled() {
		claim(c.Web.Port, "web")
	}
	if (c.Web.Username == "") != (c.Web.Password == "") {
		add("web: username and password must be set together")
	}

	names := map[string]bool{}
	for i, d := range c.Devices {
		if d == nil {
			add("devices[%d]: device must not be null", i)
			continue
		}
		id := fmt.Sprintf("devices[%d] (%q)", i, d.Name)
		if d.Name == "" {
			add("%s: name is required", id)
		} else if names[d.Name] {
			add("%s: duplicate device name", id)
		}
		names[d.Name] = true

		if d.UUID != "" && !uuidRe.MatchString(d.UUID) {
			add("%s: invalid uuid %q", id, d.UUID)
		}
		if d.MAC != "" {
			mac, err := net.ParseMAC(d.MAC)
			if err != nil || len(mac) != 6 {
				add("%s: invalid mac %q", id, d.MAC)
			}
		}
		claim(d.Ports.SOAP, id+" ports.soap")
		claim(d.Ports.RTSP, id+" ports.rtsp")

		if d.Auth != nil && (d.Auth.Username == "" || d.Auth.Password == "") {
			add("%s: auth requires both username and password", id)
		}

		if len(d.Streams) == 0 {
			add("%s: at least one stream is required", id)
			continue
		}
		streamNames := map[string]bool{}
		var primaryTarget string
		targetPorts := map[string]int{}
		portTargets := map[int]string{}
		for j, s := range d.Streams {
			if s == nil {
				add("%s streams[%d]: stream must not be null", id, j)
				continue
			}
			sid := fmt.Sprintf("%s streams[%d] (%q)", id, j, s.Name)
			if !streamNameRe.MatchString(s.Name) {
				add("%s: stream name must match %s", sid, streamNameRe)
			}
			if streamNames[s.Name] {
				add("%s: duplicate stream name", sid)
			}
			streamNames[s.Name] = true

			u, err := url.Parse(s.RTSP)
			if err != nil || u.Scheme != "rtsp" || u.Hostname() == "" {
				add("%s: rtsp must be a valid rtsp:// URL", sid)
				continue
			}
			s.u = u

			switch s.CodecName() {
			case "h264", "jpeg", "mpeg4":
			case "h265", "hevc":
				add("%s: codec %q is not supported by ONVIF Profile S; use h264, jpeg, or mpeg4",
					sid, s.Codec)
			default:
				add("%s: codec must be h264, jpeg, or mpeg4", sid)
			}
			if s.Width <= 0 || s.Height <= 0 || s.Framerate <= 0 || s.Bitrate <= 0 {
				add("%s: width/height/framerate/bitrate must be positive", sid)
			}
			if j == 0 {
				primaryTarget = s.TargetAddr()
				targetPorts[primaryTarget] = d.Ports.RTSP
				portTargets[d.Ports.RTSP] = primaryTarget
				if s.ProxyPort != 0 {
					add("%s: primary stream uses ports.rtsp, proxy_port not allowed", sid)
				}
			} else if s.TargetAddr() != primaryTarget {
				if s.ProxyPort == 0 {
					add("%s: upstream %s differs from primary %s, proxy_port is required",
						sid, s.TargetAddr(), primaryTarget)
				} else {
					target := s.TargetAddr()
					if expected, ok := targetPorts[target]; ok {
						if s.ProxyPort != expected {
							add("%s: upstream %s already uses proxy_port %d",
								sid, target, expected)
						}
						continue
					}
					if previousTarget, ok := portTargets[s.ProxyPort]; ok {
						add("%s: proxy_port %d already points to upstream %s, cannot also point to %s",
							sid, s.ProxyPort, previousTarget, target)
						continue
					}
					targetPorts[target] = s.ProxyPort
					portTargets[s.ProxyPort] = target
					claim(s.ProxyPort, sid+" proxy_port")
				}
			} else if s.ProxyPort != 0 {
				add("%s: same upstream as primary, drop proxy_port to share ports.rtsp", sid)
			}
		}

		if d.Snapshot.URL != "" {
			su, err := url.Parse(d.Snapshot.URL)
			if err != nil || (su.Scheme != "http" && su.Scheme != "https") || su.Hostname() == "" {
				add("%s: snapshot.url must be a valid http(s) URL", id)
			}
		}
		if d.Snapshot.CacheSeconds < 0 {
			add("%s: snapshot.cache_seconds must not be negative", id)
		}
		if d.Snapshot.Stream != "" && !streamNames[d.Snapshot.Stream] {
			add("%s: snapshot.stream %q not found", id, d.Snapshot.Stream)
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Problems: errs}
	}
	return nil
}

// ---- Identity generation ----------------------------------------------------

// NewUUID returns an RFC 4122 version 4 UUID.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewMAC returns a random locally-administered unicast MAC address.
func NewMAC() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[0] = (b[0] | 0x02) &^ 0x01
	parts := make([]string, 6)
	for i, x := range b {
		parts[i] = fmt.Sprintf("%02x", x)
	}
	return strings.Join(parts, ":")
}

// DetectAdvertiseIP returns the local IP used for the default route.
func DetectAdvertiseIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
