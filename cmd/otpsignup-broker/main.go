// Command otpsignup-broker runs the signed OTP-signup broker
// (internal/otpsignup): it mints a per-user provider API key with no email input
// from the user, driving the receive-only mail server (internal/otpmail) for the
// OTP. Configuration is entirely from the environment — provider- and
// host-specifics live in the deployment, not the binary.
//
//	OTPSIGNUP_LISTEN             HTTP listen addr (default 127.0.0.1:8090)
//	OTPSIGNUP_MAIL_CONTROL_URL   mail server control-API base (internal)
//	OTPSIGNUP_MAIL_TOKEN         bearer token for the mail control API
//	OTPSIGNUP_MAIL_DOMAIN        the mail domain addresses are minted under
//	OTPSIGNUP_ADDR_PREFIX        localpart prefix (default pilot_)
//	OTPSIGNUP_REGISTER_URL       provider register endpoint
//	OTPSIGNUP_VERIFY_URL         provider verify-email endpoint
//	OTPSIGNUP_KEY_PATH           dotted path to the key (default application.api_key)
//	OTPSIGNUP_DB                 sqlite ledger path
//	OTPSIGNUP_ENC_KEY            64-hex (32-byte) key sealing secrets at rest
//	OTPSIGNUP_MAX_IDS_PER_IP     per-IP distinct-caller cap (0 = unlimited)
//	OTPSIGNUP_MINT_COOLDOWN      min gap between mints from one IP, a Go duration
//	                             e.g. "30s" (0/unset = no cooldown)
package main

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/pilot-protocol/app-template/internal/otpsignup"
)

func main() {
	maxIP, _ := strconv.Atoi(os.Getenv("OTPSIGNUP_MAX_IDS_PER_IP"))
	var mintCooldown time.Duration
	if raw := os.Getenv("OTPSIGNUP_MINT_COOLDOWN"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			log.Fatalf("otpsignup-broker: OTPSIGNUP_MINT_COOLDOWN=%q: %v", raw, err)
		}
		mintCooldown = d
	}
	b, err := otpsignup.New(otpsignup.Config{
		Listen:             os.Getenv("OTPSIGNUP_LISTEN"),
		MailControlURL:     os.Getenv("OTPSIGNUP_MAIL_CONTROL_URL"),
		MailToken:          os.Getenv("OTPSIGNUP_MAIL_TOKEN"),
		MailDomain:         os.Getenv("OTPSIGNUP_MAIL_DOMAIN"),
		AddrPrefix:         os.Getenv("OTPSIGNUP_ADDR_PREFIX"),
		RegisterURL:        os.Getenv("OTPSIGNUP_REGISTER_URL"),
		VerifyURL:          os.Getenv("OTPSIGNUP_VERIFY_URL"),
		KeyPath:            os.Getenv("OTPSIGNUP_KEY_PATH"),
		DBPath:             os.Getenv("OTPSIGNUP_DB"),
		EncKeyHex:          os.Getenv("OTPSIGNUP_ENC_KEY"),
		MaxIdentitiesPerIP: maxIP,
		MintCooldown:       mintCooldown,
	})
	if err != nil {
		log.Fatalf("otpsignup-broker: %v", err)
	}
	b.SetLogger(log.New(os.Stderr, "otpsignup ", log.LstdFlags|log.LUTC))
	log.Printf("otpsignup-broker listening on %s", os.Getenv("OTPSIGNUP_LISTEN"))
	log.Fatal(b.ListenAndServe())
}
