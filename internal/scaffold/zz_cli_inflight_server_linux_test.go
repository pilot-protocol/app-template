//go:build linux

package scaffold

import "syscall"

// superviseAttr mirrors the app-store supervisor's spawn attributes on Linux:
// its own process group, and SIGKILL when the supervisor dies.
func superviseAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
