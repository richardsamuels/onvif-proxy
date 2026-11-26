package onvif

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Aiaid/onvif-proxy/internal/soap"
)

// Events service (docs/02-onvif-spec.md §6): a PullPoint-only implementation
// carrying the device's motion state, fed by SetMotion.

const (
	defaultSubscriptionTTL = 60 * time.Second
	maxSubscriptionTTL     = time.Hour
	minSubscriptionTTL     = 10 * time.Second
	maxPullTimeout         = 60 * time.Second
	defaultPullTimeout     = 10 * time.Second
	maxPullPoints          = 16
	maxQueuedEvents        = 64
	defaultMessageLimit    = 10

	topicDialectConcrete = "http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet"

	actEvents = "http://www.onvif.org/ver10/events/wsdl/EventPortType/"
	actPull   = "http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/"
	actSubMgr = "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/"

	videoSourceToken = "src"
	videoSourceCfg   = "vsc"
	analyticsCfg     = "vac"
	motionRule       = "MotionDetectorRule"
)

type motionEvent struct {
	at     time.Time
	active bool
	op     string // Initialized / Changed
}

type subscription struct {
	id      string
	ttl     time.Duration
	expires time.Time
	queue   []motionEvent
	wake    chan struct{}
}

// SetMotion updates the device's motion state. A change is queued as a
// "Changed" property event on every live pull point subscription.
func (s *Server) SetMotion(active bool) {
	now := time.Now().UTC()
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if s.motion == active {
		return
	}
	s.motion = active
	s.motionAt = now
	s.purgeLocked(now)
	for _, sub := range s.subs {
		sub.push(motionEvent{at: now, active: active, op: "Changed"})
	}
}

// Motion reports the current motion state.
func (s *Server) Motion() bool {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	return s.motion
}

func (sub *subscription) push(ev motionEvent) {
	if len(sub.queue) >= maxQueuedEvents {
		sub.queue = sub.queue[1:]
	}
	sub.queue = append(sub.queue, ev)
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}

// currentLocked is the property snapshot sent on subscribe / sync point.
func (s *Server) currentLocked(now time.Time) motionEvent {
	at := s.motionAt
	if at.IsZero() {
		at = now
	}
	return motionEvent{at: at, active: s.motion, op: "Initialized"}
}

// addLocked registers sub, evicting the pull point that expires soonest when
// the device is at its limit, and queues the current state as "Initialized".
func (s *Server) addLocked(sub *subscription, now time.Time) {
	if len(s.subs) >= maxPullPoints {
		var oldest *subscription
		for _, c := range s.subs {
			if oldest == nil || c.expires.Before(oldest.expires) {
				oldest = c
			}
		}
		delete(s.subs, oldest.id)
	}
	sub.push(s.currentLocked(now))
	s.subs[sub.id] = sub
}

func newSubscription(id string, ttl time.Duration, now time.Time) *subscription {
	return &subscription{id: id, ttl: ttl, expires: now.Add(ttl), wake: make(chan struct{}, 1)}
}

var subscriptionIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// lookupLocked returns the live subscription for id. A well-formed id that is
// unknown (lost to a restart or hot reload, or expired) is revived with the
// default TTL instead of faulting: clients such as Unifi Protect keep pulling
// the address they were given and may be slow to re-subscribe. The id itself
// is the only state needed, and the client holds it, so nothing is persisted.
// revived reports whether a fresh subscription was created.
func (s *Server) lookupLocked(id string, now time.Time) (sub *subscription, revived bool) {
	s.purgeLocked(now)
	if sub = s.subs[id]; sub != nil {
		return sub, false
	}
	if !subscriptionIDRe.MatchString(id) {
		return nil, false
	}
	sub = newSubscription(id, defaultSubscriptionTTL, now)
	s.addLocked(sub, now)
	log.Printf("onvif: device %q revived pull point %s", s.dev.Name, id)
	return sub, true
}

func (s *Server) purgeLocked(now time.Time) {
	for id, sub := range s.subs {
		if !now.Before(sub.expires) {
			delete(s.subs, id)
		}
	}
}

// ---- dispatch ---------------------------------------------------------------

// handleEvents dispatches POST /onvif/event_service. Subscription-manager
// actions sent here (instead of to the subscription address) are routed by
// the WS-Addressing To header.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	req, err := parse(w, r)
	if err != nil {
		soap.WriteInvalidArg(w, "malformed SOAP request")
		return
	}
	if !s.authorized(req, req.Action) {
		soap.WriteNotAuthorized(w)
		return
	}
	switch req.Action {
	case "GetServiceCapabilities":
		writeAction(w, actEvents+"GetServiceCapabilitiesResponse", eventServiceCapabilities())
	case "GetEventProperties":
		writeAction(w, actEvents+"GetEventPropertiesResponse", eventProperties())
	case "CreatePullPointSubscription":
		s.createPullPoint(w, r, req)
	case "PullMessages", "Renew", "Unsubscribe", "SetSynchronizationPoint":
		s.subscriptionAction(w, r, req, subscriptionIDFromAddress(req.To))
	default:
		soap.WriteActionNotSupported(w, req.Action)
	}
}

// handleSubscription dispatches POST /onvif/subscription/{id}.
func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	req, err := parse(w, r)
	if err != nil {
		soap.WriteInvalidArg(w, "malformed SOAP request")
		return
	}
	if !s.authorized(req, req.Action) {
		soap.WriteNotAuthorized(w)
		return
	}
	s.subscriptionAction(w, r, req, r.PathValue("id"))
}

func (s *Server) subscriptionAction(w http.ResponseWriter, r *http.Request, req *soap.Request, id string) {
	switch req.Action {
	case "PullMessages":
		s.pullMessages(w, r, req, id)
	case "Renew":
		s.renew(w, req, id)
	case "Unsubscribe":
		s.unsubscribe(w, id)
	case "SetSynchronizationPoint":
		s.syncPoint(w, id)
	default:
		soap.WriteActionNotSupported(w, req.Action)
	}
}

func writeAction(w http.ResponseWriter, action, body string) {
	w.Header().Set("Content-Type", soap.ContentType)
	w.WriteHeader(http.StatusOK)
	w.Write(soap.EnvelopeWithHeader("<wsa:Action>"+action+"</wsa:Action>", body))
}

func writeUnknownSubscription(w http.ResponseWriter) {
	soap.WriteFault(w, "Sender", "ter:InvalidArgVal", "Unknown or expired subscription")
}

var subscriptionPathRe = regexp.MustCompile(`/onvif/subscription/([0-9a-f]+)$`)

func subscriptionIDFromAddress(addr string) string {
	if m := subscriptionPathRe.FindStringSubmatch(addr); m != nil {
		return m[1]
	}
	return ""
}

// ---- methods ----------------------------------------------------------------

func (s *Server) createPullPoint(w http.ResponseWriter, r *http.Request, req *soap.Request) {
	now := time.Now().UTC()
	ttl := defaultSubscriptionTTL
	if v, ok := soap.ExtractElement(req.Body, "InitialTerminationTime"); ok && v != "" {
		d, ok := parseTermination(v, now)
		if !ok {
			soap.WriteInvalidArg(w, "invalid InitialTerminationTime")
			return
		}
		ttl = d
	}
	ttl = clampTTL(ttl)

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		soap.WriteFault(w, "Receiver", "ter:Action", "cannot allocate subscription")
		return
	}
	sub := newSubscription(hex.EncodeToString(raw[:]), ttl, now)

	s.evMu.Lock()
	s.purgeLocked(now)
	s.addLocked(sub, now)
	s.evMu.Unlock()

	addr := soap.XMLEscape("http://" + s.hostPort(r) + "/onvif/subscription/" + sub.id)
	writeAction(w, actEvents+"CreatePullPointSubscriptionResponse",
		"<tev:CreatePullPointSubscriptionResponse>"+
			"<tev:SubscriptionReference><wsa:Address>"+addr+"</wsa:Address></tev:SubscriptionReference>"+
			"<wsnt:CurrentTime>"+xsdTime(now)+"</wsnt:CurrentTime>"+
			"<wsnt:TerminationTime>"+xsdTime(sub.expires)+"</wsnt:TerminationTime>"+
			"</tev:CreatePullPointSubscriptionResponse>")
}

func (s *Server) pullMessages(w http.ResponseWriter, r *http.Request, req *soap.Request, id string) {
	timeout := defaultPullTimeout
	if v, ok := soap.ExtractElement(req.Body, "Timeout"); ok && v != "" {
		d, ok := parseDuration(v)
		if !ok {
			soap.WriteInvalidArg(w, "invalid Timeout")
			return
		}
		timeout = d
	}
	if timeout > maxPullTimeout {
		timeout = maxPullTimeout
	}
	limit := defaultMessageLimit
	if v, ok := soap.ExtractElement(req.Body, "MessageLimit"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			soap.WriteInvalidArg(w, "invalid MessageLimit")
			return
		}
		limit = n
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		now := time.Now().UTC()
		s.evMu.Lock()
		sub, _ := s.lookupLocked(id, now)
		if sub == nil {
			s.evMu.Unlock()
			writeUnknownSubscription(w)
			return
		}
		// A pull is proof of life: keep the pull point alive for another TTL.
		if ext := now.Add(sub.ttl); ext.After(sub.expires) {
			sub.expires = ext
		}
		if len(sub.queue) > 0 || timeout <= 0 {
			n := min(limit, len(sub.queue))
			batch := append([]motionEvent(nil), sub.queue[:n]...)
			sub.queue = sub.queue[n:]
			expires := sub.expires
			s.evMu.Unlock()
			writePullResponse(w, now, expires, batch)
			return
		}
		wake := sub.wake
		s.evMu.Unlock()

		select {
		case <-wake:
		case <-deadline.C:
			timeout = 0 // reply with whatever is queued (likely nothing)
		case <-r.Context().Done():
			return
		case <-s.stop:
			timeout = 0
		}
	}
}

func writePullResponse(w http.ResponseWriter, now, expires time.Time, batch []motionEvent) {
	var b strings.Builder
	b.WriteString("<tev:PullMessagesResponse>")
	b.WriteString("<tev:CurrentTime>" + xsdTime(now) + "</tev:CurrentTime>")
	b.WriteString("<tev:TerminationTime>" + xsdTime(expires) + "</tev:TerminationTime>")
	for _, ev := range batch {
		b.WriteString(notificationMessages(ev))
	}
	b.WriteString("</tev:PullMessagesResponse>")
	writeAction(w, actPull+"PullMessagesResponse", b.String())
}

func (s *Server) renew(w http.ResponseWriter, req *soap.Request, id string) {
	now := time.Now().UTC()
	s.evMu.Lock()
	sub, _ := s.lookupLocked(id, now)
	if sub == nil {
		s.evMu.Unlock()
		writeUnknownSubscription(w)
		return
	}
	ttl := sub.ttl
	if v, ok := soap.ExtractElement(req.Body, "TerminationTime"); ok && v != "" {
		d, ok := parseTermination(v, now)
		if !ok {
			s.evMu.Unlock()
			soap.WriteInvalidArg(w, "invalid TerminationTime")
			return
		}
		ttl = clampTTL(d)
		sub.ttl = ttl
	}
	sub.expires = now.Add(ttl)
	expires := sub.expires
	s.evMu.Unlock()

	writeAction(w, actSubMgr+"RenewResponse",
		"<wsnt:RenewResponse>"+
			"<wsnt:TerminationTime>"+xsdTime(expires)+"</wsnt:TerminationTime>"+
			"<wsnt:CurrentTime>"+xsdTime(now)+"</wsnt:CurrentTime>"+
			"</wsnt:RenewResponse>")
}

func (s *Server) unsubscribe(w http.ResponseWriter, id string) {
	s.evMu.Lock()
	_, ok := s.subs[id]
	delete(s.subs, id)
	s.evMu.Unlock()
	if !ok {
		// Idempotent: the pull point may already be gone (expired, evicted,
		// or lost to a restart/hot reload); the client's goal is met either way.
		log.Printf("onvif: device %q unsubscribe of unknown pull point %q accepted", s.dev.Name, id)
	}
	writeAction(w, actSubMgr+"UnsubscribeResponse", "<wsnt:UnsubscribeResponse/>")
}

func (s *Server) syncPoint(w http.ResponseWriter, id string) {
	now := time.Now().UTC()
	s.evMu.Lock()
	sub, revived := s.lookupLocked(id, now)
	if sub != nil && !revived { // a revived pull point already queued its state
		sub.push(s.currentLocked(now))
	}
	s.evMu.Unlock()
	if sub == nil {
		writeUnknownSubscription(w)
		return
	}
	writeAction(w, actPull+"SetSynchronizationPointResponse", "<tev:SetSynchronizationPointResponse/>")
}

// ---- rendering --------------------------------------------------------------

func eventServiceCapabilities() string {
	return "<tev:GetServiceCapabilitiesResponse>" +
		fmt.Sprintf(`<tev:Capabilities WSSubscriptionPolicySupport="false" WSPullPointSupport="true" WSPausableSubscriptionManagerInterfaceSupport="false" MaxNotificationProducers="0" MaxPullPoints="%d" PersistentNotificationStorage="false"/>`, maxPullPoints) +
		"</tev:GetServiceCapabilitiesResponse>"
}

func eventProperties() string {
	motionDesc := `<tt:MessageDescription IsProperty="true">` +
		`<tt:Source>` +
		`<tt:SimpleItemDescription Name="VideoSourceConfigurationToken" Type="tt:ReferenceToken"/>` +
		`<tt:SimpleItemDescription Name="VideoAnalyticsConfigurationToken" Type="tt:ReferenceToken"/>` +
		`<tt:SimpleItemDescription Name="Rule" Type="xs:string"/>` +
		`</tt:Source>` +
		`<tt:Data><tt:SimpleItemDescription Name="IsMotion" Type="xs:boolean"/></tt:Data>` +
		`</tt:MessageDescription>`
	alarmDesc := `<tt:MessageDescription IsProperty="true">` +
		`<tt:Source><tt:SimpleItemDescription Name="Source" Type="tt:ReferenceToken"/></tt:Source>` +
		`<tt:Data><tt:SimpleItemDescription Name="State" Type="xs:boolean"/></tt:Data>` +
		`</tt:MessageDescription>`
	return `<tev:GetEventPropertiesResponse xmlns:xs="http://www.w3.org/2001/XMLSchema">` +
		"<tev:TopicNamespaceLocation>http://www.onvif.org/onvif/ver10/topics/topicns.xml</tev:TopicNamespaceLocation>" +
		"<wsnt:FixedTopicSet>true</wsnt:FixedTopicSet>" +
		"<wstop:TopicSet>" +
		`<tns1:RuleEngine><CellMotionDetector><Motion wstop:topic="true">` + motionDesc + `</Motion></CellMotionDetector></tns1:RuleEngine>` +
		`<tns1:VideoSource><MotionAlarm wstop:topic="true">` + alarmDesc + `</MotionAlarm></tns1:VideoSource>` +
		"</wstop:TopicSet>" +
		"<wsnt:TopicExpressionDialect>http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet</wsnt:TopicExpressionDialect>" +
		"<wsnt:TopicExpressionDialect>http://docs.oasis-open.org/wsn/t-1/TopicExpression/Concrete</wsnt:TopicExpressionDialect>" +
		"<tev:MessageContentFilterDialect>http://www.onvif.org/ver10/tev/messageContentFilter/ItemFilter</tev:MessageContentFilterDialect>" +
		"<tev:MessageContentSchemaLocation>http://www.onvif.org/onvif/ver10/schema/onvif.xsd</tev:MessageContentSchemaLocation>" +
		"</tev:GetEventPropertiesResponse>"
}

// notificationMessages renders one motion event under both motion topics.
func notificationMessages(ev motionEvent) string {
	val := strconv.FormatBool(ev.active)
	msg := func(topic, source, data string) string {
		return "<wsnt:NotificationMessage>" +
			`<wsnt:Topic Dialect="` + topicDialectConcrete + `">` + topic + "</wsnt:Topic>" +
			"<wsnt:Message>" +
			`<tt:Message UtcTime="` + xsdTime(ev.at) + `" PropertyOperation="` + ev.op + `">` +
			"<tt:Source>" + source + "</tt:Source>" +
			"<tt:Data>" + data + "</tt:Data>" +
			"</tt:Message>" +
			"</wsnt:Message>" +
			"</wsnt:NotificationMessage>"
	}
	item := func(name, value string) string {
		return `<tt:SimpleItem Name="` + name + `" Value="` + value + `"/>`
	}
	return msg("tns1:RuleEngine/CellMotionDetector/Motion",
		item("VideoSourceConfigurationToken", videoSourceCfg)+
			item("VideoAnalyticsConfigurationToken", analyticsCfg)+
			item("Rule", motionRule),
		item("IsMotion", val)) +
		msg("tns1:VideoSource/MotionAlarm",
			item("Source", videoSourceToken),
			item("State", val))
}

func xsdTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// ---- xsd duration / dateTime ---------------------------------------------------

func clampTTL(d time.Duration) time.Duration {
	switch {
	case d < minSubscriptionTTL:
		return minSubscriptionTTL
	case d > maxSubscriptionTTL:
		return maxSubscriptionTTL
	}
	return d
}

// parseTermination accepts a relative xsd:duration (PT60S) or an absolute
// xsd:dateTime and returns the corresponding duration from now.
func parseTermination(v string, now time.Time) (time.Duration, bool) {
	if strings.HasPrefix(v, "P") || strings.HasPrefix(v, "-P") {
		return parseDuration(v)
	}
	t, ok := parseDateTime(v)
	if !ok {
		return 0, false
	}
	return t.Sub(now), true
}

func parseDateTime(v string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

var durationRe = regexp.MustCompile(`^P(?:(\d+)Y)?(?:(\d+)M)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// parseDuration parses a non-negative xsd:duration. Years and months are
// approximated as 365 and 30 days.
func parseDuration(v string) (time.Duration, bool) {
	m := durationRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil || v == "P" || strings.HasSuffix(v, "T") {
		return 0, false
	}
	var d time.Duration
	units := []time.Duration{365 * 24 * time.Hour, 30 * 24 * time.Hour, 24 * time.Hour, time.Hour, time.Minute}
	for i, unit := range units {
		if m[i+1] != "" {
			n, err := strconv.Atoi(m[i+1])
			if err != nil {
				return 0, false
			}
			d += time.Duration(n) * unit
		}
	}
	if m[6] != "" {
		f, err := strconv.ParseFloat(m[6], 64)
		if err != nil {
			return 0, false
		}
		d += time.Duration(f * float64(time.Second))
	}
	return d, true
}
