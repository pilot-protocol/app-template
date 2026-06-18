package publish

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"strings"
	"sync"
	"time"
)

const (
	codeTTL     = 10 * time.Minute
	tokenTTL    = 45 * time.Minute
	maxAttempts = 6
)

// CodeStore holds short-lived email-verification codes and the tokens issued
// once a code is confirmed. In-memory: codes are ephemeral by design.
type CodeStore struct {
	mu     sync.Mutex
	codes  map[string]*codeEntry // email -> code
	tokens map[string]string     // token -> email
	exp    map[string]time.Time  // token -> expiry
}

type codeEntry struct {
	code     string
	expires  time.Time
	attempts int
}

func NewCodeStore() *CodeStore {
	return &CodeStore{codes: map[string]*codeEntry{}, tokens: map[string]string{}, exp: map[string]time.Time{}}
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// Start generates and stores a 6-digit code for email, returning it (the caller
// emails it). Overwrites any prior code for that email.
func (s *CodeStore) Start(email string) string {
	code := sixDigits()
	s.mu.Lock()
	s.codes[normEmail(email)] = &codeEntry{code: code, expires: time.Now().Add(codeTTL)}
	s.mu.Unlock()
	return code
}

// Verify checks a code for email. On success it issues and returns a token.
func (s *CodeStore) Verify(email, code string) (string, bool) {
	e := normEmail(email)
	s.mu.Lock()
	defer s.mu.Unlock()
	ce := s.codes[e]
	if ce == nil || time.Now().After(ce.expires) || ce.attempts >= maxAttempts {
		return "", false
	}
	ce.attempts++
	if strings.TrimSpace(code) != ce.code {
		return "", false
	}
	delete(s.codes, e)
	tok := randToken()
	s.tokens[tok] = e
	s.exp[tok] = time.Now().Add(tokenTTL)
	return tok, true
}

// CheckToken reports whether token is valid for email (verified + not expired).
func (s *CodeStore) CheckToken(email, token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tokens[token]
	if !ok || e != normEmail(email) || time.Now().After(s.exp[token]) {
		return false
	}
	return true
}

func sixDigits() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "000000"
	}
	s := n.String()
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}

func randToken() string {
	var b [18]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
