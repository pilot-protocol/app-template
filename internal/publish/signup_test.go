package publish

import (
	"testing"
)

// signupSubmission is a byo http app whose first method self-signs-up (mints +
// caches a per-user key with no broker) and whose second method resolves that
// key from the byo header.
func signupSubmission() Submission {
	s := sampleSubmission()
	s.ID = "io.pilot.didit"
	s.Backend.BaseURL = "https://verification.didit.me/v3"
	s.Backend.Auth = "byo"
	s.Backend.Headers = []SubHeader{{Name: "x-api-key", Value: "${DIDIT_API_KEY}"}}
	s.Methods = []SubMethod{
		{Name: "didit.signup", Description: "Register with your email (sends a one-time code).", Latency: "med",
			Signup: &SubSignup{Step: "register", URL: "https://apx.didit.me/auth/v2/programmatic/register/"}},
		{Name: "didit.verify", Description: "Submit the code to mint + cache the API key.", Latency: "med",
			Signup: &SubSignup{Step: "verify", URL: "https://apx.didit.me/auth/v2/programmatic/verify-email/",
				SecretKey: "DIDIT_API_KEY"}},
		{Name: "didit.billing_balance", Description: "Check credit balance.", Latency: "fast",
			HTTP: SubRoute{Verb: "GET", Path: "/billing/balance/"}},
	}
	return s
}

func TestSignupSubmissionValidatesAndBuilds(t *testing.T) {
	s := signupSubmission()
	if errs := s.Validate(); len(errs) > 0 {
		t.Fatalf("valid signup submission rejected: %v", errs)
	}
	cfg := s.ToConfig()
	if errs := cfg.Validate(); len(errs) > 0 {
		t.Fatalf("derived config invalid: %v", errs)
	}
	if !cfg.HasSignup() {
		t.Fatal("derived config should report HasSignup()")
	}
	var reg, ver bool
	for _, m := range cfg.Methods {
		if m.Signup == nil {
			continue
		}
		if m.Signup.IsVerify() {
			ver = true
			if m.Signup.SecretKey != "DIDIT_API_KEY" {
				t.Errorf("secret key not mapped: %q", m.Signup.SecretKey)
			}
			if m.Signup.KeyPath != "application.api_key" {
				t.Errorf("key path default not applied: %q", m.Signup.KeyPath)
			}
		} else {
			reg = true
		}
	}
	if !reg || !ver {
		t.Fatal("register + verify signup routes not both carried into config")
	}
}

func TestSignupSubmissionRejectsVerifyMissingSecretKey(t *testing.T) {
	s := signupSubmission()
	s.Methods[1].Signup.SecretKey = "" // the verify method
	if errs := s.Validate(); len(errs) == 0 {
		t.Fatal("expected signup.secret_key to be required on the verify step")
	}
}
