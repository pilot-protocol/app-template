// Command firecrawl-signup-broker runs the signed Firecrawl signup broker
// (internal/firecrawlsignup): it provisions a per-user FIRECRAWL account through
// Firecrawl's Partner Integration API and returns that user's own API key, with
// no email input, no browser and no human step. Configuration is entirely from
// the environment — partner- and host-specifics live in the deployment, not the
// binary.
//
// Unlike the shared managed key it replaces, every Pilot user ends up on their
// own Firecrawl team, so each gets their own credits AND their own concurrency
// limit. After signup the adapter talks straight to api.firecrawl.dev; this
// broker is not in the data path.
//
//	FIRECRAWL_SIGNUP_LISTEN         HTTP listen addr (default 127.0.0.1:8093)
//	FIRECRAWL_SIGNUP_PARTNER_API    partner API base (default https://integrations.firecrawl.dev)
//	FIRECRAWL_SIGNUP_PARTNER_KEY    Firecrawl partner key — SERVER-SIDE ONLY, never in a bundle
//	FIRECRAWL_SIGNUP_MAIL_DOMAIN    domain we RECEIVE mail on, e.g. agents.pilotprotocol.network
//	FIRECRAWL_SIGNUP_ADDR_PREFIX    mailbox localpart prefix (default pilot-)
//	FIRECRAWL_SIGNUP_TERMS_URL      terms accepted at signup (default Firecrawl's ToS URL)
//	FIRECRAWL_SIGNUP_TERMS_REV      opaque terms revision tag recorded per account, e.g. 2026-08
//	FIRECRAWL_SIGNUP_DB             sqlite ledger path
//	FIRECRAWL_SIGNUP_ENC_KEY        64-hex (32-byte) key sealing api keys at rest
//	FIRECRAWL_SIGNUP_MAX_IDS_PER_IP per-IP distinct-caller cap (0 = unlimited)
//	FIRECRAWL_SIGNUP_COOLDOWN_MS    min gap between mints from one IP (0 = none)
//	FIRECRAWL_SIGNUP_PATH           signed request path (default /signup; set to /firecrawl/signup behind a proxy)
//
// The mail domain must be one we actually receive on: Firecrawl sends account
// notices, upgrade prompts and password resets there, and the user has to be
// able to read them.
package main

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/pilot-protocol/app-template/internal/firecrawlsignup"
)

func main() {
	maxIP, _ := strconv.Atoi(os.Getenv("FIRECRAWL_SIGNUP_MAX_IDS_PER_IP"))
	cooldownMs, _ := strconv.Atoi(os.Getenv("FIRECRAWL_SIGNUP_COOLDOWN_MS"))
	b, err := firecrawlsignup.New(firecrawlsignup.Config{
		Listen:             os.Getenv("FIRECRAWL_SIGNUP_LISTEN"),
		PartnerAPI:         os.Getenv("FIRECRAWL_SIGNUP_PARTNER_API"),
		PartnerKey:         os.Getenv("FIRECRAWL_SIGNUP_PARTNER_KEY"),
		MailDomain:         os.Getenv("FIRECRAWL_SIGNUP_MAIL_DOMAIN"),
		AddrPrefix:         os.Getenv("FIRECRAWL_SIGNUP_ADDR_PREFIX"),
		TermsURL:           os.Getenv("FIRECRAWL_SIGNUP_TERMS_URL"),
		TermsRev:           os.Getenv("FIRECRAWL_SIGNUP_TERMS_REV"),
		DBPath:             os.Getenv("FIRECRAWL_SIGNUP_DB"),
		EncKeyHex:          os.Getenv("FIRECRAWL_SIGNUP_ENC_KEY"),
		MaxIdentitiesPerIP: maxIP,
		MintCooldown:       time.Duration(cooldownMs) * time.Millisecond,
		SignupPath:         os.Getenv("FIRECRAWL_SIGNUP_PATH"),
	})
	if err != nil {
		log.Fatalf("firecrawl-signup-broker: %v", err)
	}
	b.SetLogger(log.New(os.Stderr, "firecrawlsignup ", log.LstdFlags|log.LUTC))
	log.Printf("firecrawl-signup-broker listening on %s", os.Getenv("FIRECRAWL_SIGNUP_LISTEN"))
	log.Fatal(b.ListenAndServe())
}
