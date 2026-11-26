//go:build !windows

package capability

import "os/exec"

func configureCommand(*exec.Cmd) {}
