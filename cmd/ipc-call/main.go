// Command ipc-call invokes one method on a running app-store adapter over its
// unix socket — the same way the pilot daemon brokers a call. For testing and
// ops: point it at an adapter's --socket and call a method with JSON args.
//
//	ipc-call -socket /tmp/app.sock -method partner.find-email -args '{"domain":"acme.com"}'
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/pilot-protocol/app-store/pkg/ipc"
)

func main() {
	socket := flag.String("socket", "", "adapter unix socket path")
	method := flag.String("method", "", "IPC method to call, e.g. partner.find-email")
	args := flag.String("args", "{}", "JSON args payload")
	flag.Parse()
	if *socket == "" || *method == "" {
		fmt.Fprintln(os.Stderr, "ipc-call: -socket and -method are required")
		os.Exit(2)
	}

	conn, err := net.DialTimeout("unix", *socket, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ipc-call: dial:", err)
		os.Exit(1)
	}
	defer conn.Close()

	var result json.RawMessage
	if err := ipc.Call(conn, *method, json.RawMessage(*args), &result); err != nil {
		fmt.Fprintln(os.Stderr, "ipc-call: call:", err)
		os.Exit(1)
	}
	fmt.Println(string(result))
}
