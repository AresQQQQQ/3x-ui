//go:build linux && ghostlink_smoke

package ghostlink

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
)

func TestSystemdControllerRealLifecycleSmoke(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a transient systemd unit")
	}
	binary := strings.TrimSpace(os.Getenv("GHOSTLINKD_SMOKE_BINARY"))
	if binary == "" {
		t.Skip("GHOSTLINKD_SMOKE_BINARY is not set")
	}
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("smoke binary unavailable: %v", err)
	}

	unit := fmt.Sprintf("ghostlinkd-3xui-smoke-%d.service", os.Getpid())
	unitPath := filepath.Join("/run/systemd/system", unit)
	controlDir := fmt.Sprintf("/run/ghostlinkd-3xui-smoke-%d", os.Getpid())
	controlSocket := filepath.Join(controlDir, "control.sock")
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "server.json")
	stateDBPath := filepath.Join(root, "state", "state.db")
	port := reserveDualPort(t)

	settings := validSettings(t)
	inbound := &model.Inbound{
		Id:       1,
		Protocol: model.GhostLink,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     port,
		Settings: marshalSettings(t, settings),
	}
	unitBody := fmt.Sprintf(`[Unit]
Description=GhostLink 3x-ui isolated lifecycle smoke

[Service]
Type=simple
ExecStart=%s -config %s
Restart=no
TimeoutStopSec=10s
RuntimeDirectory=%s
RuntimeDirectoryMode=0700

[Install]
WantedBy=multi-user.target
`, binary, configPath, filepath.Base(controlDir))
	if strings.ContainsAny(binary+configPath, " \t\r\n") {
		t.Fatal("smoke paths unexpectedly contain whitespace")
	}
	if err := os.WriteFile(unitPath, []byte(unitBody), 0o644); err != nil {
		t.Fatalf("write transient unit: %v", err)
	}

	runSystemctl(t, "daemon-reload")
	t.Cleanup(func() {
		_ = exec.Command("/usr/bin/systemctl", "disable", "--now", unit).Run()
		_ = os.Remove(unitPath)
		_ = exec.Command("/usr/bin/systemctl", "daemon-reload").Run()
	})

	manager := &systemctlManager{path: DefaultSystemctlBinary, unit: unit}
	controller := newSystemdController(controllerConfig{
		unitName:          unit,
		binaryPath:        binary,
		configPath:        configPath,
		stateDBPath:       stateDBPath,
		startWait:         8 * time.Second,
		pollInterval:      100 * time.Millisecond,
		preflightPorts:    true,
		controlSocket:     controlSocket,
		controlPeerUID:    uint32(os.Geteuid()),
		controlServerUID:  uint32(os.Geteuid()),
		allowTestPaths:    true,
		userOperationPath: filepath.Join(root, "config", "pending-user-op.json"),
	}, manager)

	status, err := controller.Reconcile(context.Background(), inbound)
	if err != nil {
		t.Fatalf("start lifecycle: %v", err)
	}
	if !status.Running || !status.Ready || !status.RuntimeReady ||
		status.MainPID == 0 || status.RuntimePID != status.MainPID ||
		status.InstanceID == "" ||
		status.DesiredUserRevision == "" ||
		status.RuntimeRevision != status.DesiredUserRevision ||
		status.Reason != "running" {
		t.Fatalf("started status = %#v", status)
	}
	firstPID := status.MainPID
	assertUnitEnabledState(t, unit, "enabled")
	assertPortListening(t, port)
	assertProtectedConfig(t, configPath)
	assertUnauthorizedSocketAccessDenied(t, controlSocket)

	restartedPanelController := newSystemdController(controller.cfg, manager)
	again, err := restartedPanelController.Reconcile(context.Background(), inbound)
	if err != nil {
		t.Fatalf("idempotent reconcile: %v", err)
	}
	if again.MainPID != firstPID {
		t.Fatalf("idempotent reconcile changed PID from %d to %d", firstPID, again.MainPID)
	}
	if current := systemdMainPID(t, unit); current != firstPID {
		t.Fatalf("systemd MainPID = %d, want %d", current, firstPID)
	}

	// Simulate a lost config/revision marker while the fixed unit is still
	// active. Stop must still discover and disable the hard-coded unit.
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(controller.revisionPath()); err != nil {
		t.Fatal(err)
	}
	stopped, err := controller.Stop(context.Background())
	if err != nil {
		t.Fatalf("stop lifecycle: %v", err)
	}
	if stopped.Running {
		t.Fatalf("stopped status still reports running: %#v", stopped)
	}
	assertUnitEnabledState(t, unit, "disabled")
	if fileExists(configPath) {
		t.Fatal("protected config remained after stop")
	}
	if fileExists(controlSocket) {
		t.Fatal("local control socket remained after stop")
	}
	assertPortReleased(t, port)
}

func reserveDualPort(t *testing.T) int {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcp.Addr().(*net.TCPAddr).Port
	tcp.Close()
	udp, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	udp.Close()
	return port
}

func runSystemctl(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, DefaultSystemctlBinary, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func systemdMainPID(t *testing.T, unit string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(
		ctx, DefaultSystemctlBinary, "show", unit, "--property=MainPID", "--value",
	).Output()
	if err != nil {
		t.Fatalf("read MainPID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatalf("parse MainPID %q: %v", output, err)
	}
	return pid
}

func assertUnitEnabledState(t *testing.T, unit, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, _ := exec.CommandContext(ctx, DefaultSystemctlBinary, "is-enabled", unit).CombinedOutput()
	got := strings.TrimSpace(string(output))
	if got != want {
		t.Fatalf("unit enablement = %q, want %q", got, want)
	}
}

func assertPortListening(t *testing.T, port int) {
	t.Helper()
	output := socketSummary(t)
	needle := fmt.Sprintf("127.0.0.1:%d", port)
	var tcp, udp bool
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		tcp = tcp || fields[0] == "tcp"
		udp = udp || fields[0] == "udp"
	}
	if !tcp || !udp {
		t.Fatalf("port %d is not listening on both TCP and UDP:\n%s", port, output)
	}
}

func assertPortReleased(t *testing.T, port int) {
	t.Helper()
	output := socketSummary(t)
	if strings.Contains(output, fmt.Sprintf("127.0.0.1:%d", port)) {
		t.Fatalf("port %d remained after stop:\n%s", port, output)
	}
}

func socketSummary(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/bin/ss", "-lntup").CombinedOutput()
	if err != nil {
		t.Fatalf("ss -lntup: %v: %s", err, output)
	}
	return string(output)
}

func assertProtectedConfig(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("config directory mode = %o, want 700", dirInfo.Mode().Perm())
	}
}

func assertUnauthorizedSocketAccessDenied(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	script := "import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])"
	output, err := exec.CommandContext(
		ctx,
		"runuser", "-u", "nobody", "--",
		"python3", "-c", script, path,
	).CombinedOutput()
	if err == nil {
		t.Fatalf("UID nobody unexpectedly connected to protected control socket: %s", output)
	}
}
