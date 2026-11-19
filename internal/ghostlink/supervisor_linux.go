package ghostlink

import (
	"os/exec"
	"syscall"
)

func setParentDeathSignal(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
