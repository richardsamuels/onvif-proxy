package onvif

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

const tevNS = `xmlns:tev="http://www.onvif.org/ver10/events/wsdl"`

func eventCall(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := post(t, s, path, soapEnv(wsseHeader("admin", "secret"), body))
	assertValidXML(t, rec.Body.Bytes())
	return rec
}

var addrRe = regexp.MustCompile(`<wsa:Address>http://cam\.local:8081(/onvif/subscription/[0-9a-f]+)</wsa:Address>`)

func createSub(t *testing.T, s *Server) string {
	t.Helper()
	rec := eventCall(t, s, "/onvif/event_service",
		`<tev:CreatePullPointSubscription `+tevNS+`><tev:InitialTerminationTime>PT60S</tev:InitialTerminationTime></tev:CreatePullPointSubscription>`)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreatePullPointSubscription: %d\n%s", rec.Code, rec.Body)
	}
	m := addrRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no subscription address:\n%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "<wsa:Action>http://www.onvif.org/ver10/events/wsdl/EventPortType/CreatePullPointSubscriptionResponse</wsa:Action>") {
		t.Fatalf("missing wsa:Action header:\n%s", rec.Body)
	}
	return m[1]
}

func pull(t *testing.T, s *Server, path, timeout string) string {
	t.Helper()
	rec := eventCall(t, s, path,
		`<tev:PullMessages `+tevNS+`><tev:Timeout>`+timeout+`</tev:Timeout><tev:MessageLimit>10</tev:MessageLimit></tev:PullMessages>`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PullMessages: %d\n%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestEventsAdvertised(t *testing.T) {
	s := testServer(t)
	rec := eventCall(t, s, "/onvif/device_service", `<tds:GetCapabilities><tds:Category>All</tds:Category></tds:GetCapabilities>`)
	if !strings.Contains(rec.Body.String(), "<tt:Events><tt:XAddr>http://cam.local:8081/onvif/event_service</tt:XAddr>") {
		t.Fatalf("GetCapabilities missing Events:\n%s", rec.Body)
	}
	rec = eventCall(t, s, "/onvif/device_service", `<tds:GetServices><tds:IncludeCapability>false</tds:IncludeCapability></tds:GetServices>`)
	if !strings.Contains(rec.Body.String(), "<tds:Namespace>http://www.onvif.org/ver10/events/wsdl</tds:Namespace>") {
		t.Fatalf("GetServices missing events:\n%s", rec.Body)
	}
}

func TestGetEventProperties(t *testing.T) {
	s := testServer(t)
	rec := eventCall(t, s, "/onvif/event_service", `<tev:GetEventProperties `+tevNS+`/>`)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d\n%s", rec.Code, body)
	}
	for _, want := range []string{"<CellMotionDetector><Motion wstop:topic=\"true\">", "<MotionAlarm wstop:topic=\"true\">", "Name=\"IsMotion\""} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestEventsRequireAuth(t *testing.T) {
	s := testServer(t)
	rec := post(t, s, "/onvif/event_service", soapEnv("", `<tev:GetEventProperties `+tevNS+`/>`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ter:NotAuthorized") {
		t.Fatalf("want NotAuthorized, got %d\n%s", rec.Code, rec.Body)
	}
}

func TestPullPointMotionFlow(t *testing.T) {
	s := testServer(t)
	path := createSub(t, s)

	// Initial property state is delivered first.
	body := pull(t, s, path, "PT0S")
	if !strings.Contains(body, `PropertyOperation="Initialized"`) || !strings.Contains(body, `Name="IsMotion" Value="false"`) {
		t.Fatalf("want Initialized false:\n%s", body)
	}
	if !strings.Contains(body, "tns1:RuleEngine/CellMotionDetector/Motion") || !strings.Contains(body, "tns1:VideoSource/MotionAlarm") {
		t.Fatalf("missing topics:\n%s", body)
	}

	// Long poll wakes when motion starts.
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.SetMotion(true)
	}()
	start := time.Now()
	body = pull(t, s, path, "PT5S")
	if time.Since(start) > 3*time.Second {
		t.Fatal("long poll did not wake on SetMotion")
	}
	if !strings.Contains(body, `PropertyOperation="Changed"`) || !strings.Contains(body, `Name="IsMotion" Value="true"`) || !strings.Contains(body, `Name="State" Value="true"`) {
		t.Fatalf("want Changed true:\n%s", body)
	}
	if !s.Motion() {
		t.Fatal("Motion() = false")
	}

	// Repeated state is not re-emitted; empty pull returns no messages.
	s.SetMotion(true)
	if body = pull(t, s, path, "PT0S"); strings.Contains(body, "NotificationMessage") {
		t.Fatalf("duplicate state emitted:\n%s", body)
	}

	s.SetMotion(false)
	if body = pull(t, s, path, "PT0S"); !strings.Contains(body, `Name="IsMotion" Value="false"`) {
		t.Fatalf("want false:\n%s", body)
	}

	// Sync point re-sends the current state.
	eventCall(t, s, path, `<tev:SetSynchronizationPoint `+tevNS+`/>`)
	if body = pull(t, s, path, "PT0S"); !strings.Contains(body, `PropertyOperation="Initialized"`) {
		t.Fatalf("sync point:\n%s", body)
	}

	rec := eventCall(t, s, path, `<wsnt:Renew xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"><wsnt:TerminationTime>PT120S</wsnt:TerminationTime></wsnt:Renew>`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<wsnt:RenewResponse>") {
		t.Fatalf("Renew: %d\n%s", rec.Code, rec.Body)
	}

	rec = eventCall(t, s, path, `<wsnt:Unsubscribe xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"/>`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Unsubscribe: %d\n%s", rec.Code, rec.Body)
	}
	if _, ok := s.subs[path[strings.LastIndex(path, "/")+1:]]; ok {
		t.Fatal("subscription still present after Unsubscribe")
	}
	rec = eventCall(t, s, path, `<wsnt:Unsubscribe xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"/>`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<wsnt:UnsubscribeResponse/>") {
		t.Fatalf("Unsubscribe of unknown id should succeed, got %d\n%s", rec.Code, rec.Body)
	}
}

// A pull point lost to a restart/hot reload (a fresh Server) is revived from
// the id in the address the client keeps pulling.
func TestPullPointRevivedAfterRestart(t *testing.T) {
	path := createSub(t, testServer(t))
	s := testServer(t) // new process: no subscriptions
	s.SetMotion(true)
	body := pull(t, s, path, "PT0S")
	if !strings.Contains(body, `PropertyOperation="Initialized"`) || !strings.Contains(body, `Name="IsMotion" Value="true"`) {
		t.Fatalf("revived pull should deliver current state:\n%s", body)
	}
	s.SetMotion(false)
	if body = pull(t, s, path, "PT0S"); !strings.Contains(body, `PropertyOperation="Changed"`) {
		t.Fatalf("revived subscription should receive changes:\n%s", body)
	}
	rec := eventCall(t, testServer(t), path, `<wsnt:Renew xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"><wsnt:TerminationTime>PT60S</wsnt:TerminationTime></wsnt:Renew>`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Renew of lost subscription should revive: %d\n%s", rec.Code, rec.Body)
	}
	// Sync point on a revived pull point queues the state only once.
	s2 := testServer(t)
	eventCall(t, s2, path, `<tev:SetSynchronizationPoint `+tevNS+`/>`)
	if body = pull(t, s2, path, "PT0S"); strings.Count(body, `PropertyOperation="Initialized"`) != 2 { // one event = 2 topics
		t.Fatalf("want exactly one Initialized event:\n%s", body)
	}
	// Malformed ids are still rejected.
	for _, bad := range []string{"/onvif/subscription/xyz", "/onvif/subscription/abc", "/onvif/subscription/0123456789abcdef0"} {
		rec := eventCall(t, s, bad, `<tev:PullMessages `+tevNS+`><tev:Timeout>PT0S</tev:Timeout></tev:PullMessages>`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ter:InvalidArgVal") {
			t.Fatalf("%s: want fault, got %d", bad, rec.Code)
		}
	}
}

func TestPullMessagesViaWSAddressingTo(t *testing.T) {
	s := testServer(t)
	path := createSub(t, s)
	header := strings.Replace(wsseHeader("admin", "secret"), "<s:Header>",
		`<s:Header><wsa:To xmlns:wsa="http://www.w3.org/2005/08/addressing">http://cam.local:8081`+path+`</wsa:To>`, 1)
	rec := post(t, s, "/onvif/event_service", soapEnv(header,
		`<tev:PullMessages `+tevNS+`><tev:Timeout>PT0S</tev:Timeout><tev:MessageLimit>5</tev:MessageLimit></tev:PullMessages>`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Initialized") {
		t.Fatalf("pull via event_service + wsa:To: %d\n%s", rec.Code, rec.Body)
	}
}

func TestUnknownEventActionFault(t *testing.T) {
	s := testServer(t)
	rec := eventCall(t, s, "/onvif/event_service", `<tev:Subscribe `+tevNS+`/>`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "ter:ActionNotSupported") {
		t.Fatalf("want ActionNotSupported, got %d\n%s", rec.Code, rec.Body)
	}
}

func TestPullPointEviction(t *testing.T) {
	s := testServer(t)
	first := createSub(t, s)
	for i := 0; i < maxPullPoints; i++ {
		createSub(t, s)
	}
	if got := len(s.subs); got != maxPullPoints {
		t.Fatalf("subs = %d, want %d", got, maxPullPoints)
	}
	if _, ok := s.subs[first[strings.LastIndex(first, "/")+1:]]; ok {
		t.Fatal("oldest subscription should be evicted")
	}
	// An evicted client that keeps pulling is revived without exceeding the cap.
	pull(t, s, first, "PT0S")
	if got := len(s.subs); got != maxPullPoints {
		t.Fatalf("after revive subs = %d, want %d", got, maxPullPoints)
	}
}

func TestLongPollReleasedOnShutdown(t *testing.T) {
	s := testServer(t)
	path := createSub(t, s)
	pull(t, s, path, "PT0S") // drain Initialized
	ctx, cancel := context.WithCancel(context.Background())
	ln := httptest.NewUnstartedServer(nil).Listener
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	pull(t, s, path, "PT30S")
	if time.Since(start) > 5*time.Second {
		t.Fatal("long poll not released on shutdown")
	}
	<-done
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"PT60S": 60 * time.Second, "PT1M": time.Minute, "PT1H30M": 90 * time.Minute,
		"P1D": 24 * time.Hour, "PT0.5S": 500 * time.Millisecond, "PT0S": 0,
	}
	for in, want := range cases {
		if got, ok := parseDuration(in); !ok || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "P", "PT", "60", "PTxS", "-PT5S"} {
		if _, ok := parseDuration(bad); ok {
			t.Errorf("parseDuration(%q) should fail", bad)
		}
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if d, ok := parseTermination("2026-01-01T00:02:00Z", now); !ok || d != 2*time.Minute {
		t.Errorf("absolute termination = %v, %v", d, ok)
	}
}
