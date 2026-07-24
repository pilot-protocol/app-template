// Command otpmail runs the receive-only OTP mail server (internal/otpmail):
// a catch-all SMTP receiver for one domain plus a token-authed control API the
// signup broker drives. All configuration comes from the environment — nothing
// provider- or host-specific is compiled in.
//
//	OTPMAIL_DOMAIN        the mail domain this host is MX for (required)
//	OTPMAIL_TOKEN         bearer token the control API requires (required)
//	OTPMAIL_MAILDIR       message dir, point at tmpfs (default /var/otpmail)
//	OTPMAIL_SMTP_ADDR     public SMTP listen addr (default :25)
//	OTPMAIL_CONTROL_ADDR  private control-API listen addr (default 127.0.0.1:8025)
//	OTPMAIL_MAX_CONNS         global concurrent :25 connection cap (default 256)
//	OTPMAIL_MAX_CONNS_PER_IP  per-remote-IP concurrent :25 connection cap (default 8)
package main

import (
	"log"
	"os"
	"strconv"

	"github.com/pilot-protocol/app-template/internal/otpmail"
)

func main() {
	maxBytes, _ := strconv.Atoi(os.Getenv("OTPMAIL_MAX_MSG_BYTES"))
	maxConns, _ := strconv.Atoi(os.Getenv("OTPMAIL_MAX_CONNS"))
	maxConnsPerIP, _ := strconv.Atoi(os.Getenv("OTPMAIL_MAX_CONNS_PER_IP"))
	srv, err := otpmail.New(otpmail.Config{
		Domain:        os.Getenv("OTPMAIL_DOMAIN"),
		Token:         os.Getenv("OTPMAIL_TOKEN"),
		Maildir:       os.Getenv("OTPMAIL_MAILDIR"),
		SMTPAddr:      os.Getenv("OTPMAIL_SMTP_ADDR"),
		ControlAddr:   os.Getenv("OTPMAIL_CONTROL_ADDR"),
		MaxMsgBytes:   maxBytes,
		MaxConns:      maxConns,
		MaxConnsPerIP: maxConnsPerIP,
	})
	if err != nil {
		log.Fatalf("otpmail: %v", err)
	}
	log.Fatal(srv.ListenAndServe())
}
