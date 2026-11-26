package alarm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// sample is the body an ENS NVR sends (only active channels listed).
const sample = `<?xml version="1.0" encoding="UTF-8"?>
<config version="1.0" xmlns="http://www.ipc.com/ver10">
    <alarmStatusInfo>        <motionAlarm type="boolean" id="1">true</motionAlarm>
        <motionAlarm type="boolean" id="2">true</motionAlarm>
        <motionAlarm type="boolean" id="3">true</motionAlarm>
        <motionAlarm type="boolean" id="7">true</motionAlarm>
        <motionAlarm type="boolean" id="8">true</motionAlarm>
        <motionAlarm type="boolean" id="11">true</motionAlarm>
        <motionAlarm type="boolean" id="12">true</motionAlarm>
    </alarmStatusInfo>
</config>`

type recorder struct {
	mu  sync.Mutex
	got []string
}

func (r *recorder) on(ch int, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := "off"
	if active {
		state = "on"
	}
	r.got = append(r.got, fmt.Sprintf("%02d=%s", ch, state))
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got = nil
	return out
}

func TestParseSample(t *testing.T) {
	got, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]bool{1: true, 2: true, 3: true, 7: true, 8: true, 11: true, 12: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseSpecExample(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?><config version="1.0" xmlns="http://www.ipc.com/ver10"><alarmStatusInfo>
<motionAlarm type="boolean" id="1">false</motionAlarm><motionAlarm type="boolean" id="2">TRUE</motionAlarm>
<motionAlarm type="boolean" id="0">true</motionAlarm><motionAlarm type="boolean" id="x">true</motionAlarm><motionAlarm id="5">maybe</motionAlarm>
<sensorAlarmIn type="list" count="1"><itemType type="boolean"/><item id="1">true</item></sensorAlarmIn>
</alarmStatusInfo></config>`
	got, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int]bool{1: false, 2: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseMalformed(t *testing.T) {
	for _, b := range []string{"", "not xml <", "<config><motionAlarm id='1'>true</config>"} {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("Parse(%q) should fail", b)
		}
	}
}

func TestApplySnapshotSemantics(t *testing.T) {
	var rec recorder
	s := NewServer(Options{OnChange: rec.on})

	s.Apply(map[int]bool{1: true, 3: true})
	if got, want := rec.take(), []string{"01=on", "03=on"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// Repeat: no transitions.
	s.Apply(map[int]bool{1: true, 3: true})
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("repeat emitted %v", got)
	}
	// Channel 3 omitted -> off; explicit false on 1 -> off; 2 on.
	s.Apply(map[int]bool{1: false, 2: true})
	if got, want := rec.take(), []string{"01=off", "02=on", "03=off"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := s.Active(); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("Active = %v", got)
	}
}

func TestHoldTimeout(t *testing.T) {
	var rec recorder
	now := time.Unix(1000, 0)
	s := NewServer(Options{Hold: 10 * time.Second, OnChange: rec.on})
	s.now = func() time.Time { return now }

	s.Apply(map[int]bool{4: true})
	rec.take()
	now = now.Add(5 * time.Second)
	s.Apply(map[int]bool{4: true}) // refresh
	now = now.Add(9 * time.Second)
	s.expire()
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("expired too early: %v", got)
	}
	now = now.Add(2 * time.Second)
	s.expire()
	if got, want := rec.take(), []string{"04=off"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}

	// Hold 0 disables the timeout.
	s2 := NewServer(Options{OnChange: rec.on})
	s2.now = func() time.Time { return now }
	s2.Apply(map[int]bool{1: true})
	rec.take()
	now = now.Add(time.Hour)
	s2.expire()
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("hold=0 expired: %v", got)
	}
}

func TestHandler(t *testing.T) {
	var rec recorder
	h := NewServer(Options{OnChange: rec.on}).Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/SendAlarmStatus", strings.NewReader(sample)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `status="success"`) {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if got := rec.take(); len(got) != 7 {
		t.Fatalf("transitions = %v", got)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/SendAlarmStatus", strings.NewReader("<oops")))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `errorCode="2"`) {
		t.Fatalf("malformed: got %d %s", w.Code, w.Body)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/SendAlarmStatus", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/Other", strings.NewReader(sample)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("other path: got %d", w.Code)
	}

	big := "<config>" + strings.Repeat(" ", maxBodyBytes) + "</config>"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/SendAlarmStatus", strings.NewReader(big)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversize: got %d", w.Code)
	}
}
