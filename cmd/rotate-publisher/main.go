// rotate-publisher re-points an app's publisher pin in the catalogue and opens
// the re-signed catalogue PR: the same admin-gated rotation the publish-server's
// /admin/rotate-key does (publish.RotatePublisher), for use from a workflow that
// holds CATALOG_PUBLISH_TOKEN and CATALOG_SIGN_KEY.
//
//	rotate-publisher -id io.pilot.wallet -publisher ed25519:<base64>
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/pilot-protocol/app-template/internal/publish"
)

func main() {
	id := flag.String("id", "", "app id whose publisher pin to re-point")
	pub := flag.String("publisher", "", "new publisher pin, ed25519:<base64 32-byte key>")
	flag.Parse()
	if *id == "" || *pub == "" {
		fmt.Fprintln(os.Stderr, "usage: rotate-publisher -id <app id> -publisher ed25519:<base64>")
		os.Exit(2)
	}
	url, err := publish.RotatePublisher(*id, *pub, os.Getenv("CATALOG_PUBLISH_TOKEN"), os.Getenv("CATALOG_SIGN_KEY"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "rotate:", err)
		os.Exit(1)
	}
	fmt.Println(url)
}
