// Command onvif-proxy exposes RTSP streams as virtual ONVIF Profile S
// devices. See docs/ for the full design.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Aiaid/onvif-proxy/internal/alarm"
	"github.com/Aiaid/onvif-proxy/internal/config"
	"github.com/Aiaid/onvif-proxy/internal/discovery"
	"github.com/Aiaid/onvif-proxy/internal/mediautil"
	"github.com/Aiaid/onvif-proxy/internal/onvif"
	"github.com/Aiaid/onvif-proxy/internal/rtspproxy"
	"github.com/Aiaid/onvif-proxy/internal/web"
)

var version = "dev"

var snapshotHTTPClient = &http.Client{Timeout: 15 * time.Second}

func main() {
	configPath := flag.String("config", envOr("CONFIG", "./config.yaml"), "path to config.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		if err := config.WriteDefault(*configPath); err != nil {
			log.Fatalf("generate default config: %v", err)
		}
		log.Printf("no config found — generated %s, add devices via the web UI", *configPath)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if err := os.Chmod(*configPath, 0o600); err != nil {
		log.Printf("WARNING: could not restrict config permissions to 0600: %v", err)
	}
	overrides, err := config.ApplyEnvOverrides(cfg)
	for _, o := range overrides {
		log.Printf("config: %s", o)
	}
	if err != nil {
		log.Fatalf("env overrides: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := newManager(*configPath, cfg)
	if err := m.start(ctx); err != nil {
		log.Fatalf("start: %v", err)
	}

	if cfg.Web.IsEnabled() {
		if cfg.Web.Username == "" {
			log.Print("WARNING: web UI and MCP are listening without authentication; configure web.username/password before exposing this host")
		}
		ws := web.New(cfg.Web, m)
		webLn, err := net.Listen("tcp4", ":"+strconv.Itoa(cfg.Web.Port))
		if err != nil {
			log.Fatalf("web server: %v", err)
		}
		go func() {
			if err := ws.Serve(ctx, webLn); err != nil && ctx.Err() == nil {
				log.Fatalf("web server: %v", err)
			}
		}()
		log.Printf("web UI listening on :%d", cfg.Web.Port)
	}

	<-ctx.Done()
	log.Print("shutting down")
	m.stopAll()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// manager owns the running set of virtual devices and implements web.Backend.
type manager struct {
	configPath  string
	startedAt   time.Time
	advertiseIP string

	// applyMu covers the complete prepare/stop/start/persist transaction. mu only
	// protects the currently published runtime snapshot.
	applyMu sync.Mutex
	mu      sync.RWMutex
	cfg     *config.Config
	gen     *generation
	disc    *discovery.Server

	discCancel context.CancelFunc
	discDone   chan struct{}
	rootCtx    context.Context
}

func newManager(path string, cfg *config.Config) *manager {
	adv := cfg.Server.AdvertiseIP
	if adv == "" {
		adv = config.DetectAdvertiseIP()
	}
	return &manager{
		configPath:  path,
		startedAt:   time.Now(),
		advertiseIP: adv,
		cfg:         cfg,
	}
}

// start launches discovery plus the initial device generation.
func (m *manager) start(ctx context.Context) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	gen, err := m.prepareGeneration(ctx, m.cfg, m.advertiseIP)
	if err != nil {
		return err
	}
	var pd *preparedDiscovery
	if m.cfg.Server.DiscoveryEnabled() {
		pd, err = prepareDiscovery(m.discoveryDevicesFor(m.cfg, m.advertiseIP))
		if err != nil {
			gen.closePrepared()
			return fmt.Errorf("ws-discovery: %w", err)
		}
	}

	m.mu.Lock()
	m.rootCtx = ctx
	m.gen = gen
	if pd != nil {
		m.activateDiscoveryLocked(ctx, pd)
	}
	m.mu.Unlock()
	gen.activate()
	return nil
}

type deviceState struct {
	mu      sync.RWMutex
	running bool
	err     string
}

func (s *deviceState) fail(err error) {
	s.mu.Lock()
	s.running = false
	if err != nil {
		s.err = err.Error()
	}
	s.mu.Unlock()
}

func (s *deviceState) snapshot() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running, s.err
}

type generationTask struct {
	label string
	state *deviceState
	ln    net.Listener
	run   func(context.Context) error
}

type generation struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	tasks     []generationTask
	states    map[string]*deviceState
	caches    map[string]*mediautil.Cache
	servers   map[string]*onvif.Server // by device UUID
	activated bool
}

func (m *manager) prepareGeneration(parent context.Context, cfg *config.Config, advertiseIP string) (*generation, error) {
	genCtx, cancel := context.WithCancel(parent)
	gen := &generation{
		ctx:     genCtx,
		cancel:  cancel,
		done:    make(chan struct{}),
		states:  make(map[string]*deviceState, len(cfg.Devices)),
		caches:  make(map[string]*mediautil.Cache, len(cfg.Devices)),
		servers: make(map[string]*onvif.Server, len(cfg.Devices)),
	}
	lc := net.ListenConfig{}

	for _, dev := range cfg.Devices {
		dev := dev
		state := &deviceState{running: true}
		gen.states[dev.UUID] = state
		cache := mediautil.NewCache(time.Duration(dev.SnapshotCacheSeconds()) * time.Second)
		gen.caches[dev.UUID] = cache

		soapLn, err := lc.Listen(genCtx, "tcp4", ":"+strconv.Itoa(dev.Ports.SOAP))
		if err != nil {
			gen.closePrepared()
			return nil, fmt.Errorf("device %q SOAP port %d: %w", dev.Name, dev.Ports.SOAP, err)
		}
		srv := onvif.NewServer(dev, onvif.Options{
			AdvertiseIP: advertiseIP,
			Version:     version,
			SnapshotFunc: func(ctx context.Context, streamName string) ([]byte, string, error) {
				return m.snapshot(ctx, dev, streamName, cache)
			},
		})
		gen.servers[dev.UUID] = srv
		gen.tasks = append(gen.tasks, generationTask{
			label: fmt.Sprintf("device %q SOAP server", dev.Name),
			state: state,
			ln:    soapLn,
			run: func(ctx context.Context) error {
				return srv.Serve(ctx, soapLn)
			},
		})

		for _, b := range dev.ProxyBindings() {
			b := b
			rtspLn, err := lc.Listen(genCtx, "tcp4", ":"+strconv.Itoa(b.ListenPort))
			if err != nil {
				gen.closePrepared()
				return nil, fmt.Errorf("device %q RTSP port %d: %w", dev.Name, b.ListenPort, err)
			}
			gen.tasks = append(gen.tasks, generationTask{
				label: fmt.Sprintf("device %q RTSP proxy :%d -> %s", dev.Name, b.ListenPort, b.Target),
				state: state,
				ln:    rtspLn,
				run: func(ctx context.Context) error {
					return rtspproxy.Serve(ctx, rtspLn, b.Target)
				},
			})
		}
	}

	if cfg.Alarm.Enabled {
		devices := cfg.Devices
		var warnedMu sync.Mutex
		warned := map[int]bool{}
		recv := alarm.NewServer(alarm.Options{
			Hold: time.Duration(cfg.Alarm.Hold()) * time.Second,
			OnChange: func(channel int, active bool) {
				if channel < 1 || channel > len(devices) {
					warnedMu.Lock()
					first := !warned[channel]
					warned[channel] = true
					warnedMu.Unlock()
					if first {
						log.Printf("alarm: motion channel %d has no device (devices[%d] does not exist); ignoring it", channel, channel-1)
					}
					return
				}
				dev := devices[channel-1]
				log.Printf("alarm: channel %d -> device %q motion=%v", channel, dev.Name, active)
				gen.servers[dev.UUID].SetMotion(active)
			},
		})
		if p := cfg.Alarm.Poll; p != nil {
			opts := alarm.PollOptions{
				URL:      p.URL,
				Username: p.Username,
				Password: p.Password,
				Interval: p.Interval(),
			}
			gen.tasks = append(gen.tasks, generationTask{
				label: fmt.Sprintf("alarm poller %s every %s", p.URL, opts.Interval),
				state: &deviceState{running: true},
				run: func(ctx context.Context) error {
					return recv.Poll(ctx, opts)
				},
			})
		} else {
			port := cfg.Alarm.ListenPort()
			alarmLn, err := lc.Listen(genCtx, "tcp4", ":"+strconv.Itoa(port))
			if err != nil {
				gen.closePrepared()
				return nil, fmt.Errorf("alarm receiver port %d: %w", port, err)
			}
			gen.tasks = append(gen.tasks, generationTask{
				label: fmt.Sprintf("alarm receiver :%d", port),
				state: &deviceState{running: true},
				ln:    alarmLn,
				run: func(ctx context.Context) error {
					return recv.Serve(ctx, alarmLn)
				},
			})
		}
	}
	return gen, nil
}

func (g *generation) activate() {
	g.activated = true
	var wg sync.WaitGroup
	for _, task := range g.tasks {
		task := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := task.run(g.ctx)
			if g.ctx.Err() == nil {
				if err == nil {
					err = fmt.Errorf("stopped unexpectedly")
				}
				task.state.fail(err)
				log.Printf("%s: %v", task.label, err)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(g.done)
	}()
}

func (g *generation) closePrepared() {
	g.cancel()
	for _, task := range g.tasks {
		if task.ln != nil { // pollers have no listener
			_ = task.ln.Close()
		}
	}
	if !g.activated {
		close(g.done)
	}
}

func stopGeneration(g *generation) {
	if g == nil {
		return
	}
	for _, state := range g.states {
		state.fail(nil)
	}
	g.cancel()
	select {
	case <-g.done:
	case <-time.After(5 * time.Second):
		log.Print("runtime generation did not stop within 5s")
	}
}

type preparedDiscovery struct {
	server *discovery.Server
	conn   *net.UDPConn
	addr   *net.UDPAddr
}

func prepareDiscovery(devices []discovery.Device) (*preparedDiscovery, error) {
	conn, addr, err := discovery.Listen()
	if err != nil {
		return nil, err
	}
	return &preparedDiscovery{server: discovery.New(devices), conn: conn, addr: addr}, nil
}

func (m *manager) activateDiscoveryLocked(parent context.Context, pd *preparedDiscovery) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	m.disc = pd.server
	m.discCancel = cancel
	m.discDone = done
	go func() {
		defer close(done)
		if err := pd.server.Serve(ctx, pd.conn, pd.addr); err != nil && ctx.Err() == nil {
			log.Printf("ws-discovery: %v", err)
		}
	}()
}

func stopDiscovery(cancel context.CancelFunc, done chan struct{}) {
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Print("ws-discovery did not stop within 5s")
	}
}

func (m *manager) stopAll() {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.mu.Lock()
	gen := m.gen
	cancel, done := m.discCancel, m.discDone
	m.gen = nil
	m.disc = nil
	m.discCancel = nil
	m.discDone = nil
	m.mu.Unlock()
	stopGeneration(gen)
	stopDiscovery(cancel, done)
}

func (m *manager) discoveryDevicesFor(cfg *config.Config, advertiseIP string) []discovery.Device {
	out := make([]discovery.Device, 0, len(cfg.Devices))
	for _, d := range cfg.Devices {
		out = append(out, discovery.Device{
			UUID:     d.UUID,
			Name:     d.Name,
			Hardware: d.Info.Model,
			XAddr:    fmt.Sprintf("http://%s:%d/onvif/device_service", advertiseIP, d.Ports.SOAP),
		})
	}
	return out
}

// ---- web.Backend implementation -------------------------------------------

func (m *manager) ConfigYAML() ([]byte, error) {
	return os.ReadFile(m.configPath)
}

func (m *manager) ApplyConfig(raw []byte, dryRun bool) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	cfg, persisted, err := config.Prepare(raw)
	if err != nil {
		return err
	}
	if _, err := config.ApplyEnvOverrides(cfg); err != nil {
		return err
	}

	m.mu.RLock()
	oldCfg, oldGen, oldAdv := m.cfg, m.gen, m.advertiseIP
	rootCtx := m.rootCtx
	oldDiscoveryEnabled := oldCfg.Server.DiscoveryEnabled()
	m.mu.RUnlock()
	if !sameWebRuntimeConfig(oldCfg.Web, cfg.Web) {
		return fmt.Errorf("web.enabled and web.port require a process restart; web credentials can be reloaded live")
	}
	if dryRun {
		return nil
	}

	newAdv := cfg.Server.AdvertiseIP
	if newAdv == "" {
		newAdv = config.DetectAdvertiseIP()
	}

	var pd *preparedDiscovery
	if !oldDiscoveryEnabled && cfg.Server.DiscoveryEnabled() {
		pd, err = prepareDiscovery(m.discoveryDevicesFor(cfg, newAdv))
		if err != nil {
			return fmt.Errorf("ws-discovery: %w", err)
		}
	}

	stopGeneration(oldGen)
	newGen, err := m.prepareGeneration(rootCtx, cfg, newAdv)
	if err != nil {
		if pd != nil {
			_ = pd.conn.Close()
		}
		restoreErr := m.restoreGeneration(oldCfg, oldAdv)
		if restoreErr != nil {
			return fmt.Errorf("apply failed: %v; restoring previous runtime also failed: %w", err, restoreErr)
		}
		return fmt.Errorf("apply failed, previous runtime restored: %w", err)
	}

	m.mu.Lock()
	m.cfg = cfg
	m.gen = newGen
	m.advertiseIP = newAdv
	m.mu.Unlock()
	newGen.activate()

	if err := writeConfigAtomic(m.configPath, persisted); err != nil {
		stopGeneration(newGen)
		if pd != nil {
			_ = pd.conn.Close()
		}
		restoreErr := m.restoreGeneration(oldCfg, oldAdv)
		if restoreErr != nil {
			return fmt.Errorf("persist config: %v; restoring previous runtime also failed: %w", err, restoreErr)
		}
		return fmt.Errorf("persist config failed, previous runtime restored: %w", err)
	}

	newDiscoveryEnabled := cfg.Server.DiscoveryEnabled()
	switch {
	case oldDiscoveryEnabled && newDiscoveryEnabled:
		m.mu.RLock()
		disc := m.disc
		m.mu.RUnlock()
		if disc != nil {
			disc.SetDevices(m.discoveryDevicesFor(cfg, newAdv))
		}
	case oldDiscoveryEnabled && !newDiscoveryEnabled:
		m.mu.Lock()
		cancel, done := m.discCancel, m.discDone
		m.disc = nil
		m.discCancel = nil
		m.discDone = nil
		m.mu.Unlock()
		stopDiscovery(cancel, done)
	case !oldDiscoveryEnabled && newDiscoveryEnabled:
		m.mu.Lock()
		m.activateDiscoveryLocked(rootCtx, pd)
		m.mu.Unlock()
	}

	log.Printf("config reloaded: %d device(s)", len(cfg.Devices))
	return nil
}

func (m *manager) restoreGeneration(cfg *config.Config, advertiseIP string) error {
	gen, err := m.prepareGeneration(m.rootCtx, cfg, advertiseIP)
	if err != nil {
		m.mu.Lock()
		m.cfg = cfg
		m.gen = nil
		m.advertiseIP = advertiseIP
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	m.cfg = cfg
	m.gen = gen
	m.advertiseIP = advertiseIP
	m.mu.Unlock()
	gen.activate()
	return nil
}

func sameWebRuntimeConfig(a, b config.WebConfig) bool {
	return a.IsEnabled() == b.IsEnabled() &&
		a.Port == b.Port
}

func writeConfigAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (m *manager) Devices() []web.DeviceRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]web.DeviceRuntime, 0, len(m.cfg.Devices))
	for _, d := range m.cfg.Devices {
		var running, motion bool
		var runErr string
		if m.gen != nil {
			if state := m.gen.states[d.UUID]; state != nil {
				running, runErr = state.snapshot()
			}
			if srv := m.gen.servers[d.UUID]; srv != nil {
				motion = srv.Motion()
			}
		}
		out = append(out, web.DeviceRuntime{Device: d, Running: running, Error: runErr, Motion: motion})
	}
	return out
}

// WebConfig lets the web authentication middleware observe credential changes
// from a successful hot reload. Listener-affecting fields are rejected by
// ApplyConfig and therefore remain fixed for the process lifetime.
func (m *manager) WebConfig() config.WebConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Web
}

// Snapshot serves the ONVIF GetSnapshotUri endpoint and the web UI: camera
// passthrough when snapshot.url is configured, otherwise a cached ffmpeg grab.
func (m *manager) Snapshot(ctx context.Context, dev *config.Device, streamName string) ([]byte, string, error) {
	m.mu.RLock()
	var cache *mediautil.Cache
	if m.gen != nil {
		cache = m.gen.caches[dev.UUID]
	}
	m.mu.RUnlock()
	if cache == nil {
		return nil, "", fmt.Errorf("device %q is not running", dev.Name)
	}
	return m.snapshot(ctx, dev, streamName, cache)
}

func (m *manager) snapshot(ctx context.Context, dev *config.Device, streamName string, cache *mediautil.Cache) ([]byte, string, error) {
	if dev.Snapshot.URL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, dev.Snapshot.URL, nil)
		if err != nil {
			return nil, "", err
		}
		resp, err := snapshotHTTPClient.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("snapshot passthrough: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("snapshot passthrough: upstream %s", resp.Status)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, (10<<20)+1))
		if err != nil {
			return nil, "", err
		}
		if len(data) > 10<<20 {
			return nil, "", fmt.Errorf("snapshot passthrough: upstream image exceeds 10 MiB")
		}
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "image/jpeg"
		}
		return data, ct, nil
	}

	stream := dev.SnapshotStream()
	if streamName != "" {
		if s := dev.StreamByName(streamName); s != nil {
			stream = s
		}
	}
	data, err := cache.Get(stream.Name, func() ([]byte, error) {
		return mediautil.Grab(ctx, stream.SourceURL())
	})
	if err != nil {
		return nil, "", err
	}
	return data, "image/jpeg", nil
}

func (m *manager) DiscoveryLog() []discovery.LogEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.disc == nil {
		return nil
	}
	return m.disc.Log()
}

func (m *manager) Status() web.Status {
	m.mu.RLock()
	advertiseIP := m.advertiseIP
	m.mu.RUnlock()
	return web.Status{
		Version:       version,
		AdvertiseIP:   advertiseIP,
		UptimeSeconds: int64(time.Since(m.startedAt).Seconds()),
		FFmpeg:        mediautil.Available(),
	}
}
