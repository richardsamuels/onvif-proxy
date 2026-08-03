// Package discovery implements the ONVIF WS-Discovery (2005/04 draft) responder:
// a single UDP socket bound to :3702 and joined to the 239.255.255.250 multicast
// group answers Probe requests, and announces the proxy's virtual devices via
// Hello (on start / add) and Bye (on shutdown / removal).
package discovery

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"sync"
	"time"
)

// Device is the minimal identity this package advertises for one virtual
// device. XAddr is the full device service URL, e.g.
// "http://192.168.1.10:8000/onvif/device_service".
type Device struct {
	UUID     string
	Name     string
	Hardware string
	XAddr    string // full URL
}

// LogEntry records one discovery event for the web UI. Kind is one of
// probe/hello/bye/match.
type LogEntry struct {
	Time   time.Time
	Remote string
	Kind   string
	Detail string
}

const (
	// logCap is the size of the in-memory ring buffer surfaced by Log.
	logCap = 50

	probeWorkers   = 16
	probeQueueSize = 64
)

// Server is the WS-Discovery responder. It is safe for concurrent use.
type Server struct {
	mu        sync.Mutex
	devices   []Device
	conn      *net.UDPConn // active only while Run is executing
	multicast *net.UDPAddr
	entries   []LogEntry // newest first, capped at logCap
}

// New creates a responder for the given devices. Announcements are not sent
// until Run binds the socket.
func New(devices []Device) *Server {
	return &Server{devices: append([]Device(nil), devices...)}
}

// Run binds the multicast socket, sends a Hello for every current device, then
// serves Probe requests until ctx is cancelled, at which point it sends a Bye
// for every current device and returns ctx.Err().
func (s *Server) Run(ctx context.Context) error {
	conn, addr, err := Listen()
	if err != nil {
		return err
	}
	return s.Serve(ctx, conn, addr)
}

// Listen binds and joins the ONVIF WS-Discovery multicast group. Callers may
// use it before replacing a running configuration so bind failures are known
// synchronously.
func Listen() (*net.UDPConn, *net.UDPAddr, error) {
	addr, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		return nil, nil, err
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		return nil, nil, err
	}
	return conn, addr, nil
}

// Serve runs discovery on a socket returned by Listen. Serve owns conn and
// closes it on shutdown.
func (s *Server) Serve(ctx context.Context, conn *net.UDPConn, addr *net.UDPAddr) error {
	if conn == nil || addr == nil {
		return errors.New("discovery: nil UDP connection or multicast address")
	}
	s.mu.Lock()
	s.conn = conn
	s.multicast = addr
	devs := append([]Device(nil), s.devices...)
	s.mu.Unlock()

	for _, d := range devs {
		if _, err := conn.WriteToUDP(buildHello(d), addr); err == nil {
			s.addLog("", "hello", d.UUID)
		}
	}

	done := make(chan error, 1)
	go func() { done <- s.readLoop(conn) }()

	select {
	case <-ctx.Done():
		// Announce departure for whatever set is current at shutdown.
		s.mu.Lock()
		byeDevs := append([]Device(nil), s.devices...)
		if s.conn == conn {
			s.conn = nil
			s.multicast = nil
		}
		s.mu.Unlock()
		for _, d := range byeDevs {
			if _, err := conn.WriteToUDP(buildBye(d), addr); err == nil {
				s.addLog("", "bye", d.UUID)
			}
		}
		_ = conn.Close()
		<-done
		return ctx.Err()
	case err := <-done:
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
			s.multicast = nil
		}
		s.mu.Unlock()
		_ = conn.Close()
		if err == nil {
			return errors.New("discovery: UDP read loop stopped")
		}
		return err
	}
}

// SetDevices hot-reloads the device set. Removed devices get a Bye and added
// devices get a Hello. A metadata change for an existing UUID is announced as
// Bye(old) followed by Hello(new), so clients do not retain stale XAddrs or
// scopes.
func (s *Server) SetDevices(devices []Device) {
	s.mu.Lock()
	removed, added := diffDevices(s.devices, devices)
	s.devices = append([]Device(nil), devices...)
	conn, addr := s.conn, s.multicast
	s.mu.Unlock()

	// Not running yet: the table is replaced and Run will Hello on start.
	if conn == nil {
		return
	}
	for _, d := range removed {
		if _, err := conn.WriteToUDP(buildBye(d), addr); err == nil {
			s.addLog("", "bye", d.UUID)
		}
	}
	for _, d := range added {
		if _, err := conn.WriteToUDP(buildHello(d), addr); err == nil {
			s.addLog("", "hello", d.UUID)
		}
	}
}

func diffDevices(old, current []Device) (removed, added []Device) {
	oldByUUID := make(map[string]Device, len(old))
	for _, d := range old {
		oldByUUID[d.UUID] = d
	}
	newByUUID := make(map[string]Device, len(current))
	for _, d := range current {
		newByUUID[d.UUID] = d
	}
	for _, d := range old {
		next, exists := newByUUID[d.UUID]
		if !exists || next != d {
			removed = append(removed, d)
		}
	}
	for _, d := range current {
		previous, exists := oldByUUID[d.UUID]
		if !exists || previous != d {
			added = append(added, d)
		}
	}
	return removed, added
}

// Log returns up to the last logCap events, newest first.
func (s *Server) Log() []LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LogEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

type probeJob struct {
	data []byte
	src  *net.UDPAddr
}

// readLoop receives datagrams until the socket is closed. A fixed worker pool
// and bounded queue prevent a UDP flood from creating unbounded goroutines.
func (s *Server) readLoop(conn *net.UDPConn) error {
	jobs := make(chan probeJob, probeQueueSize)
	var workers sync.WaitGroup
	workers.Add(probeWorkers)
	for range probeWorkers {
		go func() {
			defer workers.Done()
			for job := range jobs {
				s.handleProbe(conn, job.data, job.src)
			}
		}()
	}

	buf := make([]byte, 65535)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			close(jobs)
			workers.Wait()
			return err
		}
		if len(jobs) == cap(jobs) {
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case jobs <- probeJob{data: data, src: src}:
		default:
			// Saturated: drop this datagram. UDP discovery is retryable, and
			// bounded memory is preferable to queue growth under a flood.
		}
	}
}

// handleProbe answers a single incoming datagram if it is a matching Probe,
// unicasting one ProbeMatches message per device back to the prober.
func (s *Server) handleProbe(conn *net.UDPConn, data []byte, src *net.UDPAddr) {
	messageID, types, ok := parseProbe(data)
	if !ok {
		return // not a Probe (e.g. our own Hello/Bye looped back)
	}
	s.addLog(src.String(), "probe", messageID)
	if !typesMatch(types) {
		return
	}

	// WS-Discovery APP_MAX_DELAY: random delay before replying.
	time.Sleep(time.Duration(rand.Intn(maxDelayMS+1)) * time.Millisecond)

	s.mu.Lock()
	devs := append([]Device(nil), s.devices...)
	s.mu.Unlock()
	for _, d := range devs {
		if _, err := conn.WriteToUDP(buildProbeMatches(d, messageID), src); err == nil {
			s.addLog(src.String(), "match", d.UUID)
		}
	}
}

// addLog prepends an event, capping the ring at logCap.
func (s *Server) addLog(remote, kind, detail string) {
	e := LogEntry{Time: time.Now(), Remote: remote, Kind: kind, Detail: detail}
	s.mu.Lock()
	s.entries = append([]LogEntry{e}, s.entries...)
	if len(s.entries) > logCap {
		s.entries = s.entries[:logCap]
	}
	s.mu.Unlock()
}
