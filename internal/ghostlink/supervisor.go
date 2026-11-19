package ghostlink

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func NewDefaultController() Controller {
	if !InContainer() {
		return NewDefaultSystemdController()
	}
	uid := uint32(max(os.Geteuid(), 0))
	config := filepath.Join(ContainerRoot, "server.json")
	return newSystemdController(controllerConfig{
		unitName:          DefaultSystemdUnit,
		binaryPath:        filepath.Join(ContainerRoot, "ghostlinkd"),
		configPath:        config,
		stateDBPath:       filepath.Join(ContainerRoot, "state.db"),
		controlSocket:     DefaultControlSocketPath,
		controlPeerUID:    uid,
		controlServerUID:  uid,
		preflightPorts:    true,
		userOperationPath: filepath.Join(ContainerRoot, "pending-user-op.json"),
		container:         true,
	}, &processManager{binary: filepath.Join(ContainerRoot, "ghostlinkd"), config: config})
}

func ShutdownContainer(ctx context.Context, controller Controller) error {
	if c, ok := controller.(*systemdController); ok && c.cfg.container {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.manager.DisableNow(ctx)
	}
	return nil
}

type child struct {
	cmd  *exec.Cmd
	done chan struct{}
}

type processManager struct {
	binary, config string
	mu             sync.Mutex
	child          *child
}

func (m *processManager) Available() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.child != nil
}

func (m *processManager) Snapshot(context.Context) (unitSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot(), nil
}

func (m *processManager) snapshot() unitSnapshot {
	if m.child != nil {
		select {
		case <-m.child.done:
			m.child = nil
		default:
			return unitSnapshot{Loaded: true, Active: true, PID: m.child.cmd.Process.Pid}
		}
	}
	return unitSnapshot{Loaded: true}
}

func (m *processManager) EnableNow(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot().Active {
		return nil
	}
	return m.start(ctx)
}

func (m *processManager) DisableNow(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stop(ctx)
}

func (m *processManager) Restart(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.stop(ctx); err != nil {
		return err
	}
	return m.start(ctx)
}

func (m *processManager) start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	command := exec.Command(m.binary, "-config", m.config)
	setParentDeathSignal(command)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	command.Env = append(os.Environ(), "GODEBUG="+extendGoDebug(os.Getenv("GODEBUG")), "GOMEMLIMIT="+memoryBudget())
	if err := command.Start(); err != nil {
		return err
	}
	p := &child{cmd: command, done: make(chan struct{})}
	m.child = p
	go func() {
		_ = p.cmd.Wait()
		close(p.done)
	}()
	return nil
}

func (m *processManager) stop(ctx context.Context) error {
	if !m.snapshot().Active {
		return nil
	}
	p := m.child
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = p.cmd.Process.Kill()
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-ctx.Done():
		_ = p.cmd.Process.Kill()
		return ctx.Err()
	case <-timer.C:
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.child = nil
	return nil
}

func extendGoDebug(existing string) string {
	values := []string{}
	for _, value := range strings.Split(existing, ",") {
		if value != "" && !strings.HasPrefix(value, "http2xconnect=") {
			values = append(values, value)
		}
	}
	return strings.Join(append(values, "http2xconnect=1"), ",")
}

func memoryBudget() string {
	if limit := os.Getenv("GHOSTLINK_GOMEMLIMIT"); limit != "" {
		return limit
	}
	return "96MiB"
}
