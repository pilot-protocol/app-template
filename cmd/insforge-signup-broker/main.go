// Command insforge-signup-broker runs the signed InsForge signup broker
// (internal/insforgesignup): it provisions a per-user InsForge project under one
// managed master account and returns the project's access key, with no email and
// no browser. Configuration is entirely from the environment — account- and
// host-specifics live in the deployment, not the binary.
//
//	INSFORGE_SIGNUP_LISTEN         HTTP listen addr (default 127.0.0.1:8092)
//	INSFORGE_SIGNUP_TOKEN_URL      OAuth token endpoint (https://api.insforge.dev/api/oauth/v1/token)
//	INSFORGE_SIGNUP_CLIENT_ID      OAuth client id the refresh token was issued to
//	INSFORGE_SIGNUP_REFRESH_TOKEN  master account refresh token
//	INSFORGE_SIGNUP_PLATFORM_API   platform API base (https://api.insforge.dev)
//	INSFORGE_SIGNUP_ORG_ID         org new projects are created under
//	INSFORGE_SIGNUP_REGION         project region (default us-east)
//	INSFORGE_SIGNUP_BACKEND_DOMAIN backend host suffix (default insforge.app)
//	INSFORGE_SIGNUP_PROJECT_PREFIX project name prefix (default pilot-)
//	INSFORGE_SIGNUP_DB             sqlite ledger path
//	INSFORGE_SIGNUP_ENC_KEY        64-hex (32-byte) key sealing the project key at rest
//	INSFORGE_SIGNUP_MAX_IDS_PER_IP per-IP distinct-caller cap (0 = unlimited)
//	INSFORGE_SIGNUP_PATH           signed request path (default /signup; set to /insforge/signup behind a proxy)
package main

import (
	"log"
	"os"
	"strconv"

	"github.com/pilot-protocol/app-template/internal/insforgesignup"
)

func main() {
	maxIP, _ := strconv.Atoi(os.Getenv("INSFORGE_SIGNUP_MAX_IDS_PER_IP"))
	b, err := insforgesignup.New(insforgesignup.Config{
		Listen:             os.Getenv("INSFORGE_SIGNUP_LISTEN"),
		TokenURL:           os.Getenv("INSFORGE_SIGNUP_TOKEN_URL"),
		ClientID:           os.Getenv("INSFORGE_SIGNUP_CLIENT_ID"),
		RefreshToken:       os.Getenv("INSFORGE_SIGNUP_REFRESH_TOKEN"),
		PlatformAPI:        os.Getenv("INSFORGE_SIGNUP_PLATFORM_API"),
		OrgID:              os.Getenv("INSFORGE_SIGNUP_ORG_ID"),
		Region:             os.Getenv("INSFORGE_SIGNUP_REGION"),
		BackendDomain:      os.Getenv("INSFORGE_SIGNUP_BACKEND_DOMAIN"),
		ProjectPrefix:      os.Getenv("INSFORGE_SIGNUP_PROJECT_PREFIX"),
		DBPath:             os.Getenv("INSFORGE_SIGNUP_DB"),
		EncKeyHex:          os.Getenv("INSFORGE_SIGNUP_ENC_KEY"),
		MaxIdentitiesPerIP: maxIP,
		SignupPath:         os.Getenv("INSFORGE_SIGNUP_PATH"),
	})
	if err != nil {
		log.Fatalf("insforge-signup-broker: %v", err)
	}
	b.SetLogger(log.New(os.Stderr, "insforgesignup ", log.LstdFlags|log.LUTC))
	log.Printf("insforge-signup-broker listening on %s", os.Getenv("INSFORGE_SIGNUP_LISTEN"))
	log.Fatal(b.ListenAndServe())
}
