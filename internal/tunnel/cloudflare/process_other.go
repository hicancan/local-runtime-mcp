//go:build !windows

package cloudflare

import "os/exec"

// configureCommand applies platform-specific child-process settings.
func configureCommand(*exec.Cmd) {}
