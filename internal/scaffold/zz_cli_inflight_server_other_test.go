//go:build !linux && !windows

package scaffold

import "syscall"

// superviseAttr mirrors the app-store supervisor's spawn attributes outside
// Linux: its own process group only (there is no parent-death signal).
func superviseAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
