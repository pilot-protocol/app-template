package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type capSink struct{ events []AccessEvent }

func (c *capSink) Emit(e AccessEvent) { c.events = append(c.events, e) }

// fixedNow returns a clock that advances by one fixed step per call, so
// DurationMs is deterministic (start=T0, end=T0+step).
func fixedNow(base time.Time, step time.Duration) func() time.Time {
	n := -1
	return func() time.Time {
		n++
		if n == 0 {
			return base
		}
		return base.Add(step)
	}
}

// TestAccessLog_EmitsPerForwardedRequest: a routed request produces exactly one
// event carrying the app id, method-path, status, byte counts, the pilotctl
// command from X-Pilot-Method, and a duration.
func TestAccessLog_EmitsPerForwardedRequest(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	sink := &capSink{}
	h := WithAccessLog(inner, "pilot-broker", sink, fixedNow(time.Unix(1_800_000_000, 0), 7*time.Millisecond))

	req := httptest.NewRequest("POST", "/io.pilot.agentphone/v1/messages", strings.NewReader(`{"x":1}`))
	req.Header.Set(PilotMethodHeader, "agentphone.send_message")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	e := sink.events[0]
	if e.Broker != "pilot-broker" || e.AppID != "io.pilot.agentphone" || e.Path != "/v1/messages" {
		t.Errorf("bad routing fields: %+v", e)
	}
	if e.HTTPMethod != "POST" || e.Status != 200 {
		t.Errorf("bad http fields: %+v", e)
	}
	if e.Method != "agentphone.send_message" {
		t.Errorf("X-Pilot-Method not captured: %q", e.Method)
	}
	if e.DurationMs != 7 {
		t.Errorf("DurationMs = %d, want 7", e.DurationMs)
	}
	if e.RespBytes == 0 {
		t.Error("RespBytes not recorded")
	}
}

// TestAccessLog_SkipsUnroutedAndHealth: /gw/ health and requests the broker
// flagged X-Pilot-Unrouted (unknown app / bad route) emit nothing, so scanner
// noise never counts as broker traffic.
func TestAccessLog_SkipsUnroutedAndHealth(t *testing.T) {
	unrouted := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(UnroutedHeader, "1")
		w.WriteHeader(404)
	})
	sink := &capSink{}
	WithAccessLog(unrouted, "b", sink, nil).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/HNAP1", nil))
	if len(sink.events) != 0 {
		t.Errorf("unrouted request logged: %+v", sink.events)
	}

	health := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	WithAccessLog(health, "b", sink, nil).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/gw/health", nil))
	if len(sink.events) != 0 {
		t.Errorf("/gw/health logged: %+v", sink.events)
	}
}

// TestStdoutSink_PrefixedJSONLine: the sink writes one AccessLogPrefix-tagged
// JSON line, which the crawler selects out of the mixed journald stream.
func TestStdoutSink_PrefixedJSONLine(t *testing.T) {
	var buf strings.Builder
	StdoutSink{W: &buf}.Emit(AccessEvent{Broker: "b", AppID: "io.pilot.x", Status: 200})
	out := buf.String()
	if !strings.HasPrefix(out, AccessLogPrefix) || !strings.HasSuffix(out, "\n") {
		t.Fatalf("bad line framing: %q", out)
	}
	var e AccessEvent
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(out), AccessLogPrefix)), &e); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if e.AppID != "io.pilot.x" {
		t.Errorf("round-trip lost app_id: %+v", e)
	}
}
