// Command broker-sign signs a broker request the way the Pilot daemon does, for
// testing and ops. It loads (or generates) an ed25519 identity and prints the
// X-Pilot-* headers for a given method+path+body — feed them straight to curl.
//
//	# generate a throwaway identity and sign a call:
//	broker-sign -gen-key alice.key -method POST -path /io.pilot.sixtyfour/enrich -body '{"q":"acme"}'
//
//	# emit curl-ready -H args:
//	broker-sign -key alice.key -method GET -path /io.pilot.x/ping -curl
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pilot-protocol/app-template/internal/broker"
)

func main() {
	keyPath := flag.String("key", "", "ed25519 identity file (base64 private key)")
	genKey := flag.String("gen-key", "", "generate a new identity, write it to this path, and use it")
	method := flag.String("method", "POST", "HTTP method")
	path := flag.String("path", "", "request path the broker verifies, e.g. /io.pilot.x/ping")
	body := flag.String("body", "", "request body")
	curl := flag.Bool("curl", false, "print as curl -H arguments")
	flag.Parse()

	if *path == "" {
		fmt.Fprintln(os.Stderr, "broker-sign: -path is required")
		os.Exit(2)
	}

	priv, err := identity(*keyPath, *genKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "broker-sign:", err)
		os.Exit(1)
	}

	headers := broker.Sign(priv, *method, *path, []byte(*body), time.Now())
	order := []string{broker.HdrCaller, broker.HdrTimestamp, broker.HdrSignature}
	if *curl {
		var b strings.Builder
		for _, k := range order {
			fmt.Fprintf(&b, "-H '%s: %s' ", k, headers[k])
		}
		fmt.Println(strings.TrimSpace(b.String()))
		return
	}
	for _, k := range order {
		fmt.Printf("%s: %s\n", k, headers[k])
	}
}

func identity(keyPath, genKey string) (ed25519.PrivateKey, error) {
	if genKey != "" {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}
		enc := base64.RawStdEncoding.EncodeToString(priv)
		if err := os.WriteFile(genKey, []byte(enc+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write key: %w", err)
		}
		return priv, nil
	}
	if keyPath == "" {
		return nil, fmt.Errorf("need -key or -gen-key")
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	dec, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if len(dec) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("key must be %d bytes, got %d", ed25519.PrivateKeySize, len(dec))
	}
	return ed25519.PrivateKey(dec), nil
}
