// Package otpmail is a receive-only mail server for collecting one-time-code
// (OTP) emails during a provider signup, plus a small token-authenticated
// control API a signup broker drives.
//
// It exists for the broker-signup pattern: to obtain an API key from a provider
// that gates it behind an email OTP, the broker provisions a mailbox on a domain
// we control, registers it with the provider, and reads the OTP the provider
// emails. This package is the mailbox side — it ONLY receives (never sends, never
// relays), so there is no SPF/DKIM/PTR/reputation to manage; it just needs the
// domain's MX pointed at it and TCP :25 reachable.
//
// Design points:
//   - Catch-all for one configured domain; RCPT for any other domain is refused.
//   - A message is kept ONLY if its recipient localpart is currently provisioned
//     (an active signup window). Mail to unknown/torn-down addresses is accepted
//     at the SMTP layer then discarded, so the server is not a spam sink.
//   - The OTP is a live secret for seconds: store under a private maildir
//     (point it at tmpfs), return the parsed code once, and delete on read or
//     teardown. The code and message body are NEVER logged.
//   - The control API (provision / otp / teardown) is bearer-token authed and is
//     meant to listen on a private interface reachable only by the broker.
package otpmail

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Config configures a Server. All fields come from the deployment environment;
// nothing provider- or host-specific is baked in.
type Config struct {
	Domain        string // the mail domain we are MX for, e.g. "sub.example.net"
	Maildir       string // dir for per-recipient message files (point at tmpfs)
	SMTPAddr      string // public SMTP listen address, default ":25"
	ControlAddr   string // private control-API listen address, default "127.0.0.1:8025"
	Token         string // bearer token the control API requires (required)
	MaxMsgBytes   int    // per-message cap (default 256 KiB)
	MaxConns      int    // global concurrent :25 connection cap (default 256, DoS guard)
	MaxConnsPerIP int    // per-remote-IP concurrent :25 connection cap (default 8, DoS guard)
}

// Server is a receive-only SMTP catch-all plus a control API.
type Server struct {
	cfg    Config
	mu     sync.Mutex
	active map[string]time.Time // provisioned localpart@domain -> provisioned-at
	log    *log.Logger

	// Connection admission control for the public :25 listener — an unauthenticated
	// endpoint reachable by anyone, so it needs its own DoS guards independent of
	// the per-message/recipient checks below: a global slot semaphore plus a
	// per-remote-IP cap, so no single source can exhaust goroutines/fds and starve
	// everyone else.
	connSem  chan struct{}
	connMu   sync.Mutex
	connByIP map[string]int
}

// New validates cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Domain == "" {
		return nil, fmt.Errorf("otpmail: Domain is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("otpmail: control Token is required")
	}
	if cfg.Maildir == "" {
		cfg.Maildir = "/var/otpmail"
	}
	if cfg.SMTPAddr == "" {
		cfg.SMTPAddr = ":25"
	}
	if cfg.ControlAddr == "" {
		cfg.ControlAddr = "127.0.0.1:8025"
	}
	if cfg.MaxMsgBytes <= 0 {
		cfg.MaxMsgBytes = 256 << 10
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 256
	}
	if cfg.MaxConnsPerIP <= 0 {
		cfg.MaxConnsPerIP = 8
	}
	// The control-plane channel between the signup broker and this mail server is
	// bearer-token auth over a private interface, not mTLS (a larger deferred
	// design change — see docs/BROKER-SIGNUP.md#security-notes). Until then,
	// enforce a floor on token strength so a weak/guessable shared secret isn't
	// the actual security boundary.
	if len(cfg.Token) < minTokenLen {
		return nil, fmt.Errorf("otpmail: control Token must be at least %d characters (use a long random bearer token)", minTokenLen)
	}
	cfg.Domain = strings.ToLower(cfg.Domain)
	if err := os.MkdirAll(cfg.Maildir, 0o700); err != nil {
		return nil, fmt.Errorf("otpmail: maildir: %w", err)
	}
	return &Server{
		cfg:      cfg,
		active:   map[string]time.Time{},
		log:      log.New(os.Stderr, "otpmail ", log.LstdFlags|log.LUTC),
		connSem:  make(chan struct{}, cfg.MaxConns),
		connByIP: map[string]int{},
	}, nil
}

// minTokenLen is the minimum length required of the control-API bearer token.
const minTokenLen = 20

// ListenAndServe starts the SMTP + control listeners and blocks until one fails.
func (s *Server) ListenAndServe() error {
	errc := make(chan error, 2)
	go func() { errc <- s.serveSMTP() }()
	go func() { errc <- s.serveControl() }()
	return <-errc
}

// ── provisioning registry ───────────────────────────────────────────────────

func (s *Server) provision(addr string) {
	addr = strings.ToLower(addr)
	s.mu.Lock()
	s.active[addr] = time.Now()
	s.mu.Unlock()
	s.log.Printf("provision %s", addr)
}

func (s *Server) isActive(addr string) bool {
	s.mu.Lock()
	_, ok := s.active[strings.ToLower(addr)]
	s.mu.Unlock()
	return ok
}

func (s *Server) teardown(addr string) {
	addr = strings.ToLower(addr)
	s.mu.Lock()
	delete(s.active, addr)
	s.mu.Unlock()
	_ = os.RemoveAll(s.mailboxDir(addr))
	s.log.Printf("teardown %s", addr)
}

func (s *Server) mailboxDir(addr string) string {
	return filepath.Join(s.cfg.Maildir, safeLocal(strings.ToLower(addr)))
}

// safeLocal maps an address to a filesystem-safe directory name (no traversal).
func safeLocal(addr string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '@', r == '.', r == '_', r == '-', r == '+':
			return r
		default:
			return '_'
		}
	}, addr)
}

// ── SMTP receiver ───────────────────────────────────────────────────────────

func (s *Server) serveSMTP() error {
	ln, err := net.Listen("tcp", s.cfg.SMTPAddr)
	if err != nil {
		return fmt.Errorf("otpmail: smtp listen %s: %w", s.cfg.SMTPAddr, err)
	}
	s.log.Printf("smtp receive-only catch-all for @%s on %s", s.cfg.Domain, s.cfg.SMTPAddr)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go s.acceptSMTP(c)
	}
}

// acceptSMTP applies connection admission control (a global cap plus a
// per-remote-IP cap) before handing the connection to handleSMTP, then
// releases its slot(s) once handling finishes. :25 is unauthenticated and
// internet-facing, so without this a single source (or a botnet) can open
// unbounded connections and exhaust goroutines/file descriptors — a cheap DoS
// against the one mailbox this VM exists to serve.
func (s *Server) acceptSMTP(c net.Conn) {
	ip := remoteIP(c)
	if !s.acquireConn(ip) {
		s.log.Printf("reject smtp connection from %s: connection cap reached", ip)
		c.Close()
		return
	}
	defer s.releaseConn(ip)
	s.handleSMTP(c)
}

// acquireConn reserves one global slot and one per-IP slot, atomically enough
// that a rejected per-IP attempt gives its global slot back immediately (it
// must not leak a slot on the rejection path). Returns false if either cap is
// currently exhausted.
func (s *Server) acquireConn(ip string) bool {
	select {
	case s.connSem <- struct{}{}:
	default:
		return false // global concurrent-connection cap reached
	}
	s.connMu.Lock()
	if s.connByIP[ip] >= s.cfg.MaxConnsPerIP {
		s.connMu.Unlock()
		<-s.connSem
		return false // per-IP concurrent-connection cap reached
	}
	s.connByIP[ip]++
	s.connMu.Unlock()
	return true
}

func (s *Server) releaseConn(ip string) {
	s.connMu.Lock()
	if s.connByIP[ip] <= 1 {
		delete(s.connByIP, ip)
	} else {
		s.connByIP[ip]--
	}
	s.connMu.Unlock()
	<-s.connSem
}

// remoteIP strips the port from a connection's remote address (falling back to
// the raw address if it isn't host:port, e.g. some test/pipe conns).
func remoteIP(c net.Conn) string {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return host
}

func (s *Server) handleSMTP(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	br := bufio.NewReader(c)
	w := func(code string) { fmt.Fprintf(c, "%s\r\n", code) }
	w("220 " + s.cfg.Domain + " ESMTP")

	var rcpts []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "HELO", "EHLO":
			w("250 " + s.cfg.Domain)
		case "MAIL":
			w("250 2.1.0 OK")
		case "RCPT":
			addr := strings.ToLower(extractAngle(line))
			if strings.HasSuffix(addr, "@"+s.cfg.Domain) {
				rcpts = append(rcpts, addr) // accept for our domain; kept only if active
				w("250 2.1.5 OK")
			} else {
				w("550 5.7.1 relaying denied")
			}
		case "DATA":
			if len(rcpts) == 0 {
				w("503 5.5.1 need RCPT")
				continue
			}
			w("354 End data with <CR><LF>.<CR><LF>")
			data, err := s.readData(br)
			if err != nil {
				return
			}
			for _, r := range rcpts {
				s.deliver(r, data)
			}
			w("250 2.0.0 OK")
			rcpts = nil
		case "RSET":
			rcpts = nil
			w("250 2.0.0 OK")
		case "NOOP":
			w("250 2.0.0 OK")
		case "QUIT":
			w("221 2.0.0 Bye")
			return
		default:
			w("250 2.0.0 OK")
		}
	}
}

func (s *Server) readData(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == ".\r\n" || line == ".\n" {
			return buf.Bytes(), nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:] // undo dot-stuffing
		}
		if buf.Len() < s.cfg.MaxMsgBytes {
			buf.WriteString(line)
		}
	}
}

// deliver stores a message ONLY if its recipient is currently provisioned; mail
// to unknown/torn-down addresses is dropped so we are not a spam sink. The body
// is never logged.
func (s *Server) deliver(rcpt string, data []byte) {
	if !s.isActive(rcpt) {
		s.log.Printf("drop %d bytes for unprovisioned %s", len(data), rcpt)
		return
	}
	dir := s.mailboxDir(rcpt)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	name := filepath.Join(dir, fmt.Sprintf("%d.eml", time.Now().UnixNano()))
	_ = os.WriteFile(name, data, 0o600)
	s.log.Printf("delivered %d bytes for %s", len(data), rcpt) // bytes only, never the body/code
}

func extractAngle(s string) string {
	if i, j := strings.IndexByte(s, '<'), strings.IndexByte(s, '>'); i >= 0 && j > i {
		return s[i+1 : j]
	}
	if k := strings.LastIndexByte(s, ':'); k >= 0 {
		return strings.TrimSpace(s[k+1:])
	}
	return strings.TrimSpace(s)
}

// ── OTP extraction ──────────────────────────────────────────────────────────

var otpKeyword = regexp.MustCompile(`(?is)(?:code|otp|verification|pin)\b.{0,40}?\b([0-9A-Za-z]{6})\b`)
var otpAny = regexp.MustCompile(`\b([0-9A-Za-z]{6})\b`)

// ReadOTP returns the OTP for a provisioned address if one has been delivered,
// deleting the message(s) once read. Empty string means "not yet". The code is
// never logged.
func (s *Server) ReadOTP(addr string) string {
	dir := s.mailboxDir(addr)
	files, _ := filepath.Glob(filepath.Join(dir, "*.eml"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if code := ParseOTP(string(b)); code != "" {
			_ = os.Remove(f)
			return code
		}
	}
	return ""
}

// ParseOTP extracts a 6-char code, preferring one near a keyword and mixing
// letters+digits (provider codes look like A3K9F2).
func ParseOTP(body string) string {
	if m := otpKeyword.FindStringSubmatch(body); len(m) == 2 && mixed(m[1]) {
		return strings.ToUpper(m[1])
	}
	for _, m := range otpAny.FindAllStringSubmatch(body, -1) {
		if mixed(m[1]) {
			return strings.ToUpper(m[1])
		}
	}
	if m := otpKeyword.FindStringSubmatch(body); len(m) == 2 {
		return strings.ToUpper(m[1])
	}
	return ""
}

func mixed(s string) bool {
	var l, d bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			d = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			l = true
		}
	}
	return l && d
}

// ── control API ─────────────────────────────────────────────────────────────

func (s *Server) serveControl() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/provision", s.auth(s.hProvision))
	mux.HandleFunc("/otp", s.auth(s.hOTP))
	mux.HandleFunc("/teardown", s.auth(s.hTeardown))
	s.log.Printf("control api on %s", s.cfg.ControlAddr)
	return http.ListenAndServe(s.cfg.ControlAddr, mux)
}

// auth requires a constant-time-compared bearer token.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	want := "Bearer " + s.cfg.Token
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) addrFromReq(r *http.Request) string {
	if a := r.URL.Query().Get("addr"); a != "" {
		return a
	}
	var body struct {
		Addr string `json:"addr"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.Addr
}

// validAddr guards against injection: localpart@domain, our domain only.
func (s *Server) validAddr(addr string) bool {
	addr = strings.ToLower(addr)
	if !strings.HasSuffix(addr, "@"+s.cfg.Domain) {
		return false
	}
	local := strings.TrimSuffix(addr, "@"+s.cfg.Domain)
	if local == "" || len(local) > 64 {
		return false
	}
	for _, r := range local {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' || r == '+') {
			return false
		}
	}
	return true
}

func (s *Server) hProvision(w http.ResponseWriter, r *http.Request) {
	addr := s.addrFromReq(r)
	if !s.validAddr(addr) {
		http.Error(w, "bad addr", http.StatusBadRequest)
		return
	}
	s.provision(addr)
	writeJSON(w, map[string]any{"ok": true, "addr": strings.ToLower(addr)})
}

func (s *Server) hOTP(w http.ResponseWriter, r *http.Request) {
	addr := s.addrFromReq(r)
	if !s.validAddr(addr) || !s.isActive(addr) {
		http.Error(w, "not provisioned", http.StatusBadRequest)
		return
	}
	code := s.ReadOTP(addr)
	writeJSON(w, map[string]any{"ok": true, "ready": code != "", "code": code})
}

func (s *Server) hTeardown(w http.ResponseWriter, r *http.Request) {
	addr := s.addrFromReq(r)
	if !s.validAddr(addr) {
		http.Error(w, "bad addr", http.StatusBadRequest)
		return
	}
	s.teardown(addr)
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
