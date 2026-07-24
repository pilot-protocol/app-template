package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// AccessEvent is one broker request, emitted per request for monitoring. The
// JSON tags are the wire/DB contract — keep them stable.
type AccessEvent struct {
	TS         time.Time `json:"ts"`
	Broker     string    `json:"broker"`
	AppID      string    `json:"app_id"`
	HTTPMethod string    `json:"http_method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"duration_ms"`
	ReqBytes   int64     `json:"req_bytes"`
	RespBytes  int64     `json:"resp_bytes"`
	Method     string    `json:"method,omitempty"` // pilotctl command, from X-Pilot-Method
	Source     string    `json:"source,omitempty"`
}

// AccessSink receives one event per request; MUST NOT block the request path.
type AccessSink interface{ Emit(AccessEvent) }

// AccessLogPrefix marks stdout access lines so the crawler can select them out
// of the broker's mixed journald stream.
const AccessLogPrefix = "ACCESS "

// PilotMethodHeader carries the pilotctl command (`<app>.<method>`) an app
// adapter stamps on a brokered call, making stats command-oriented.
const PilotMethodHeader = "X-Pilot-Method"

// UnroutedHeader is set by the broker on requests it could not route to a
// managed app (unknown app / bad route); the access log skips these so
// internet-scanner noise never counts as broker traffic.
const UnroutedHeader = "X-Pilot-Unrouted"

// StdoutSink writes one prefixed JSON line per event to W (os.Stdout in prod).
type StdoutSink struct{ W io.Writer }

func (s StdoutSink) Emit(e AccessEvent) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	line := make([]byte, 0, len(AccessLogPrefix)+len(b)+1)
	line = append(line, AccessLogPrefix...)
	line = append(line, b...)
	line = append(line, '\n')
	_, _ = s.W.Write(line) // single Write so concurrent goroutines don't interleave
}

// statusRecorder captures the response status code and byte count.
type statusRecorder struct {
	http.ResponseWriter
	status int
	nbytes int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.nbytes += int64(n)
	return n, err
}

// appIDFromPath splits /<app-id>/<rest> exactly as ServeHTTP does.
func appIDFromPath(p string) (appID, method string) {
	appID, rest, ok := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	if !ok {
		return appID, "/"
	}
	return appID, "/" + rest
}

// WithAccessLog wraps next, emitting one AccessEvent per forwarded request to
// sink. broker labels the process; now is injectable for tests (nil → time.Now).
// /gw/ paths, a nil sink, and requests the broker flagged X-Pilot-Unrouted pass
// through unlogged.
func WithAccessLog(next http.Handler, broker string, sink AccessSink, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sink == nil || strings.HasPrefix(r.URL.Path, "/gw/") {
			next.ServeHTTP(w, r)
			return
		}
		start := now()
		pilotMethod := r.Header.Get(PilotMethodHeader)
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		if rec.Header().Get(UnroutedHeader) != "" { // scanner noise / unknown app
			return
		}
		appID, method := appIDFromPath(r.URL.Path)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		reqBytes := r.ContentLength
		if reqBytes < 0 {
			reqBytes = 0
		}
		sink.Emit(AccessEvent{
			TS:         start,
			Broker:     broker,
			AppID:      appID,
			HTTPMethod: r.Method,
			Path:       method,
			Method:     pilotMethod,
			Status:     rec.status,
			DurationMs: now().Sub(start).Milliseconds(),
			ReqBytes:   reqBytes,
			RespBytes:  rec.nbytes,
		})
	})
}
