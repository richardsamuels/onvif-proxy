// Package alarm implements the TVT / ENS XML alarm-status protocol: devices
// either POST their alarm status to /SendAlarmStatus (push) or are polled with
// GetAlarmStatus (poll). The Server tracks per-channel motion state and reports
// transitions through a callback. See docs/08-alarm-server.md.
package alarm

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxBodyBytes      = 64 << 10
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	idleTimeout       = 60 * time.Second
	expiryInterval    = time.Second
	pollTimeout       = 5 * time.Second
	defaultPollEvery  = time.Second

	respSuccess   = `<?xml version="1.0" encoding="UTF-8"?><config status="success"/>`
	respBadFormat = `<?xml version="1.0" encoding="UTF-8"?><config status="failed" errorCode="2"/>`
)

// Options configures a Server.
type Options struct {
	// Hold clears a channel that has not been re-reported active for this
	// long. 0 disables the timeout.
	Hold time.Duration
	// OnChange is called (without internal locks held) for every channel
	// state transition. channel is the 1-based motionAlarm id.
	OnChange func(channel int, active bool)
}

// Server receives SendAlarmStatus pushes and tracks motion per channel.
type Server struct {
	opts Options
	now  func() time.Time

	mu       sync.Mutex
	lastSeen map[int]time.Time // active channels -> last time reported true
}

// NewServer returns a receiver with all channels inactive.
func NewServer(opts Options) *Server {
	return &Server{opts: opts, now: time.Now, lastSeen: make(map[int]time.Time)}
}

// ErrNoStatus is returned by Parse for a well-formed document that has no
// alarmStatusInfo element (a heartbeat, or a failed-result document). Such a
// document carries no motion state and must not be applied.
var ErrNoStatus = errors.New("alarm: document has no alarmStatusInfo")

// Parse extracts every motionAlarm id -> state pair from a SendAlarmStatus
// body or GetAlarmStatus response. Elements match by local name; entries with
// an invalid id or value are skipped. A body that is not well-formed XML is an
// error; one without alarmStatusInfo returns ErrNoStatus.
func Parse(body []byte) (map[int]bool, error) {
	out := make(map[int]bool)
	dec := xml.NewDecoder(bytes.NewReader(body))
	sawRoot, sawStatus := false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		sawRoot = true
		if se.Name.Local == "alarmStatusInfo" {
			sawStatus = true
		}
		if se.Name.Local != "motionAlarm" {
			continue
		}
		var text string
		if err := dec.DecodeElement(&text, &se); err != nil {
			return nil, err
		}
		id := 0
		for _, a := range se.Attr {
			if a.Name.Local == "id" {
				id, _ = strconv.Atoi(strings.TrimSpace(a.Value))
			}
		}
		if id <= 0 {
			continue
		}
		v, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(text)))
		if err != nil {
			continue
		}
		out[id] = v
	}
	if !sawRoot {
		return nil, errors.New("alarm: empty document")
	}
	if !sawStatus {
		return nil, ErrNoStatus
	}
	return out, nil
}

// Apply merges one status snapshot: channels reported true become active,
// every other channel becomes inactive. OnChange fires for each transition.
func (s *Server) Apply(motion map[int]bool) {
	now := s.now()
	type change struct {
		ch     int
		active bool
	}
	var changes []change

	s.mu.Lock()
	for ch, active := range motion {
		if !active {
			continue
		}
		if _, was := s.lastSeen[ch]; !was {
			changes = append(changes, change{ch, true})
		}
		s.lastSeen[ch] = now
	}
	for ch := range s.lastSeen {
		if !motion[ch] {
			delete(s.lastSeen, ch)
			changes = append(changes, change{ch, false})
		}
	}
	s.mu.Unlock()

	sort.Slice(changes, func(i, j int) bool { return changes[i].ch < changes[j].ch })
	for _, c := range changes {
		s.notify(c.ch, c.active)
	}
}

// expire clears active channels whose last report is older than Hold.
func (s *Server) expire() {
	if s.opts.Hold <= 0 {
		return
	}
	cutoff := s.now().Add(-s.opts.Hold)
	var cleared []int
	s.mu.Lock()
	for ch, seen := range s.lastSeen {
		if seen.Before(cutoff) {
			delete(s.lastSeen, ch)
			cleared = append(cleared, ch)
		}
	}
	s.mu.Unlock()
	sort.Ints(cleared)
	for _, ch := range cleared {
		s.notify(ch, false)
	}
}

func (s *Server) notify(ch int, active bool) {
	if s.opts.OnChange != nil {
		s.opts.OnChange(ch, active)
	}
}

// Active returns the sorted list of currently active channels.
func (s *Server) Active() []int {
	s.mu.Lock()
	out := make([]int, 0, len(s.lastSeen))
	for ch := range s.lastSeen {
		out = append(out, ch)
	}
	s.mu.Unlock()
	sort.Ints(out)
	return out
}

// Handler returns the HTTP handler serving POST /SendAlarmStatus.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/SendAlarmStatus", s.handleSendAlarmStatus)
	return mux
}

func (s *Server) handleSendAlarmStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var motion map[int]bool
	if err == nil {
		motion, err = Parse(body)
	}
	w.Header().Set("Content-Type", "application/xml")
	if err != nil && !errors.Is(err, ErrNoStatus) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, respBadFormat)
		return
	}
	if err == nil {
		s.Apply(motion)
	}
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, respSuccess)
}

// Serve serves on an already-bound listener and runs the hold-timeout loop
// until ctx is cancelled. Serve owns ln and closes it during shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    16 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	ticker := time.NewTicker(expiryInterval)
	defer ticker.Stop()
	for {
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
		case <-ticker.C:
			s.expire()
		}
	}
}

// PollOptions configures Poll.
type PollOptions struct {
	// URL is the device API base URL; requests go to URL + "/GetAlarmStatus".
	URL string
	// Username/Password are sent as HTTP Basic auth when Username is set.
	Username, Password string
	// Interval between poll starts; <= 0 means 1s.
	Interval time.Duration
	// Client overrides the HTTP client (tests); nil uses a 5s-timeout client.
	Client *http.Client
}

// Poll fetches GetAlarmStatus every Interval and applies each status document
// as a snapshot, and runs the hold-timeout loop, until ctx is cancelled. A
// failed poll changes no state; failures are logged on the first failure and
// on recovery only.
func (s *Server) Poll(ctx context.Context, opts PollOptions) error {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultPollEvery
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: pollTimeout}
	}
	endpoint := strings.TrimRight(opts.URL, "/") + "/GetAlarmStatus"

	pollTick := time.NewTicker(interval)
	defer pollTick.Stop()
	expiryTick := time.NewTicker(expiryInterval)
	defer expiryTick.Stop()

	var lastErr string
	healthy := true
	for {
		err := s.pollOnce(ctx, client, endpoint, opts)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case err != nil && (healthy || err.Error() != lastErr):
			log.Printf("alarm: poll %s failed: %v", endpoint, err)
			healthy, lastErr = false, err.Error()
		case err == nil && !healthy:
			log.Printf("alarm: poll %s recovered", endpoint)
			healthy, lastErr = true, ""
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-expiryTick.C:
				s.expire()
			case <-pollTick.C:
				break wait
			}
		}
	}
}

func (s *Server) pollOnce(ctx context.Context, client *http.Client, endpoint string, opts PollOptions) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	if opts.Username != "" {
		req.SetBasicAuth(opts.Username, opts.Password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxBodyBytes {
		return fmt.Errorf("response larger than %d bytes", maxBodyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	motion, err := Parse(body)
	if err != nil {
		return fmt.Errorf("%w: %s", err, snippet(body))
	}
	s.Apply(motion)
	return nil
}

// snippet returns a short single-line excerpt of a response body for logs.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return strconv.Quote(s)
}
