//go:build !windows

package cloudflare

import "os/exec"

func configureCommand(*exec.Cmd) {}
