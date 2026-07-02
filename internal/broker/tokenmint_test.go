package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenMint_RegistryWiring(t *testing.T) {
	regJSON := `[{
		"id":"io.pilot.smol","upstream":"https://api.smolmachines.com","key_env":"SMOL_MASTER",
		"provision":{"provider":"tokenmint","secret_env":"SMOL_SECRET","admin_key_env":"SMOL_ADMIN"}
	}]`
	// Missing admin key → fail closed.
	_, err := ParseRegistry([]byte(regJSON), func(k string) string {
		switch k {
		case "SMOL_MASTER":
			return "smk"
		case "SMOL_SECRET":
			return "sec"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "admin key") {
		t.Fatalf("tokenmint without admin key must fail closed, got %v", err)
	}

	// With an admin key → provider builds and reports its name.
	reg, err := ParseRegistry([]byte(regJSON), func(k string) string {
		switch k {
		case "SMOL_MASTER":
			return "smk"
		case "SMOL_SECRET":
			return "sec"
		case "SMOL_ADMIN":
			return "admin-key"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("tokenmint with admin key should parse: %v", err)
	}
	app := reg.Get("io.pilot.smol")
	if app.provider == nil || app.provider.Name() != "tokenmint" {
		t.Fatalf("expected tokenmint provider, got %v", app.provider)
	}
	// Dormant: data-plane calls are inert until the live shapes are finalized.
	if _, err := app.provider.Push(context.Background(), "owner", PushSpec{}, nil); err != errProviderDormant {
		t.Fatalf("Push should be dormant, got %v", err)
	}
}

func TestTokenMint_MintUserToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tokens" || r.Header.Get("Authorization") != "Bearer admin-key" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"missing scope: admin"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "key-" + body["subject"].(string)})
	}))
	defer srv.Close()

	p := newTokenMintProvider(srv.URL, "admin-key")
	tok, err := p.mintUserToken(context.Background(), "alicepubkey")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "key-alicepubkey" {
		t.Fatalf("unexpected minted token %q", tok)
	}
}
