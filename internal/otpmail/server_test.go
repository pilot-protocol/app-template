package otpmail

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTest(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{Domain: "mx.example.net", Token: "secret-tok", Maildir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestParseOTP(t *testing.T) {
	cases := map[string]string{
		"Your verification code is A3K9F2. It expires soon.": "A3K9F2",
		"Code: BPQM3W":                          "BPQM3W",
		"no code here, just words and stuff":    "", // no mixed 6-char token
		"login now at HELLOWORLD or use 7F2K9A": "7F2K9A",
	}
	for body, want := range cases {
		if got := ParseOTP(body); got != want {
			t.Errorf("ParseOTP(%q)=%q want %q", body, got, want)
		}
	}
}

func TestControlAPIRequiresToken(t *testing.T) {
	s := newTest(t)
	ts := httptest.NewServer(http.HandlerFunc(s.auth(s.hProvision)))
	defer ts.Close()
	// no token → 401
	resp, _ := http.Post(ts.URL, "application/json", strings.NewReader(`{"addr":"a@mx.example.net"}`))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status=%d want 401", resp.StatusCode)
	}
}

func TestProvisionDeliverReadTeardown(t *testing.T) {
	s := newTest(t)
	addr := "pilot_abc@mx.example.net"

	// unprovisioned mail is dropped
	s.deliver(addr, []byte("Subject: x\r\n\r\nyour code is A3K9F2\r\n"))
	if got := s.ReadOTP(addr); got != "" {
		t.Fatalf("expected drop before provision, got %q", got)
	}

	// provision → deliver → read parses + deletes
	s.provision(addr)
	s.deliver(addr, []byte("Subject: Verify\r\n\r\nYour verification code is A3K9F2.\r\n"))
	if got := s.ReadOTP(addr); got != "A3K9F2" {
		t.Fatalf("ReadOTP=%q want A3K9F2", got)
	}
	if got := s.ReadOTP(addr); got != "" {
		t.Fatalf("expected message consumed after read, got %q", got)
	}

	// teardown removes state
	s.provision(addr)
	s.deliver(addr, []byte("Subject: x\r\n\r\ncode BPQM3W\r\n"))
	s.teardown(addr)
	if s.isActive(addr) {
		t.Fatal("expected inactive after teardown")
	}
	if got := s.ReadOTP(addr); got != "" {
		t.Fatalf("expected mailbox gone after teardown, got %q", got)
	}
}

func TestValidAddrRejectsForeignDomainAndInjection(t *testing.T) {
	s := newTest(t)
	bad := []string{"a@evil.com", "a b@mx.example.net", "../etc@mx.example.net", "@mx.example.net"}
	for _, a := range bad {
		if s.validAddr(a) {
			t.Errorf("validAddr(%q) should be false", a)
		}
	}
	if !s.validAddr("pilot_x1@mx.example.net") {
		t.Error("validAddr should accept a normal provisioned addr")
	}
}

// TestSMTPEndToEnd drives a real SMTP session over a socket and confirms the
// message lands (and only for a provisioned recipient) + the OTP reads back.
func TestSMTPEndToEnd(t *testing.T) {
	s := newTest(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handleSMTP(c)
		}
	}()
	addr := "pilot_smtp@mx.example.net"
	s.provision(addr)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	read := func() string { l, _ := br.ReadString('\n'); return l }
	send := func(l string) { fmt.Fprintf(c, "%s\r\n", l) }
	read() // 220 greeting
	send("EHLO test")
	read()
	send("MAIL FROM:<noreply@didit.me>")
	read()
	send(fmt.Sprintf("RCPT TO:<%s>", addr))
	if r := read(); !strings.HasPrefix(r, "250") {
		t.Fatalf("RCPT not accepted: %q", r)
	}
	send("DATA")
	read()
	send("Subject: Your Didit code")
	send("")
	send("Your verification code is Z8K3Q1")
	send(".")
	if r := read(); !strings.HasPrefix(r, "250") {
		t.Fatalf("DATA not accepted: %q", r)
	}
	send("QUIT")
	read()

	if got := s.ReadOTP(addr); got != "Z8K3Q1" {
		t.Fatalf("OTP via SMTP=%q want Z8K3Q1", got)
	}
}

func TestControlOTPRoundTrip(t *testing.T) {
	s := newTest(t)
	provision := httptest.NewServer(http.HandlerFunc(s.auth(s.hProvision)))
	otp := httptest.NewServer(http.HandlerFunc(s.auth(s.hOTP)))
	defer provision.Close()
	defer otp.Close()
	addr := "pilot_ctl@mx.example.net"

	do := func(url, body string) *http.Response {
		req, _ := http.NewRequest("POST", url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret-tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	do(provision.URL, `{"addr":"`+addr+`"}`)
	s.deliver(addr, []byte("Subject: v\r\n\r\ncode: Q1W2E9\r\n"))

	resp := do(otp.URL, `{"addr":"`+addr+`"}`)
	var out struct {
		Ready bool   `json:"ready"`
		Code  string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !out.Ready || out.Code != "Q1W2E9" {
		t.Fatalf("control /otp got ready=%v code=%q", out.Ready, out.Code)
	}
}
