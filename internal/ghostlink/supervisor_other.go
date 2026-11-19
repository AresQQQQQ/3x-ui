//go:build !linux

package ghostlink

import "os/exec"

func setParentDeathSignal(*exec.Cmd) {}
