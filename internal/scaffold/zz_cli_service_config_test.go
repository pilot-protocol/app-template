package scaffold

import (
	"strings"
	"testing"
)

// TestCLIServiceConfig: cli.tools / cli.service are validated so a spec cannot
// declare a service a caller could still start daemonized (no tools allowlist),
// or a readiness wait the method timeout would cut short.
func TestCLIServiceConfig(t *testing.T) {
	base := `
id: io.pilot.svc
app_version: 0.1.0
description: "svc"
namespace: svc
backend:
  type: cli
  command: ["svc"]
methods:
  - name: svc.m
    summary: "m"
`
	cases := []struct {
		name, method, want string
	}{
		{"ok enumerated", `    cli: {args: ["serve", "${port}"], service: {ready_tcp: "127.0.0.1:${port}", force_args: ["--daemonize", "no"]}}`, ""},
		{"ok passthrough", `    cli: {passthrough: true, tools: ["a", "b"], service: {tools: ["a"], ready_after: "1s"}}`, ""},
		{"tools on enumerated", `    cli: {args: ["x"], tools: ["a"]}`, "cli.tools only applies to a passthrough route"},
		{"tool with a path", `    cli: {passthrough: true, tools: ["../bin/sh"]}`, "must be a bare tool name"},
		{"passthrough service without service.tools", `    cli: {passthrough: true, tools: ["a"], service: {ready_after: "1s"}}`, "needs tools"},
		{"passthrough service without cli.tools", `    cli: {passthrough: true, service: {tools: ["a"]}}`, "needs cli.tools"},
		{"service tool outside cli.tools", `    cli: {passthrough: true, tools: ["a"], service: {tools: ["b"]}}`, "is not in cli.tools"},
		{"ready_tcp on passthrough", `    cli: {passthrough: true, tools: ["a"], service: {tools: ["a"], ready_tcp: "127.0.0.1:1"}}`, "ready_tcp/log_file need"},
		{"bad ready_tcp", `    cli: {args: ["x"], service: {ready_tcp: "nohostport"}}`, "must be host:port"},
		{"bad duration", `    cli: {args: ["x"], service: {ready_after: "soon"}}`, "not a positive Go duration"},
		{"ready_timeout past method timeout", `    cli: {args: ["x"], service: {ready_timeout: "90s"}}`, "must be below the method timeout"},
		{"service.tools on enumerated", `    cli: {args: ["x"], service: {tools: ["a"]}}`, "tools only applies to a passthrough route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(base + tc.method + "\n"))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			cfg.Resolve()
			var msgs []string
			for _, e := range cfg.Validate() {
				msgs = append(msgs, e.Error())
			}
			all := strings.Join(msgs, "\n")
			if tc.want == "" {
				for _, m := range msgs {
					if strings.Contains(m, "cli.") {
						t.Errorf("unexpected cli error: %s", m)
					}
				}
				return
			}
			if !strings.Contains(all, tc.want) {
				t.Errorf("errors %q do not mention %q", all, tc.want)
			}
		})
	}
}
