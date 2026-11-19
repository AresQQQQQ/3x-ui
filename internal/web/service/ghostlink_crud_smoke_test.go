//go:build linux && ghostlink_crud_smoke

package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/web/runtime"
)

const crudSmokeAliceSecret = "ghostlink-crud-alice-secret-old-000001"
const crudSmokeAliceNewSecret = "ghostlink-crud-alice-secret-new-000002"
const crudSmokeBobSecret = "ghostlink-crud-bob-secret-0000000003"
const crudSmokeCharlieSecret = "ghostlink-crud-charlie-secret-000004"

func TestGhostLinkServiceLiveSingleClientCRUDSmoke(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to install an isolated systemd unit")
	}
	ghostlinkd := strings.TrimSpace(os.Getenv("GHOSTLINKD_SMOKE_BINARY"))
	probe := strings.TrimSpace(os.Getenv("GHOSTLINK_USER_PROBE_BINARY"))
	if ghostlinkd == "" || probe == "" {
		t.Skip("GHOSTLINKD_SMOKE_BINARY and GHOSTLINK_USER_PROBE_BINARY are required")
	}
	for _, binary := range []string{ghostlinkd, probe} {
		info, err := os.Stat(binary)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("smoke binary unavailable: %s: %v", binary, err)
		}
	}

	setupConflictDB(t)
	root := t.TempDir()
	certPath, keyPath := ghostLinkTestCertificate(t)
	port := crudSmokeDualPort(t)
	echo := crudSmokeEcho(t)
	configPath := filepath.Join(root, "config", "server.json")
	stateDBPath := filepath.Join(root, "state", "state.db")
	operationPath := filepath.Join(root, "config", "pending-user-op.json")
	controlDir := fmt.Sprintf("/run/ghostlinkd-3xui-smoke-crud-%d", os.Getpid())
	controlSocket := filepath.Join(controlDir, "control.sock")
	unit := fmt.Sprintf("ghostlinkd-3xui-smoke-crud-%d.service", os.Getpid())
	unitPath := filepath.Join("/run/systemd/system", unit)
	unitBody := fmt.Sprintf(`[Unit]
Description=GhostLink 3x-ui service CRUD smoke

[Service]
Type=simple
ExecStart=%s -config %s
Restart=no
TimeoutStopSec=10s
RuntimeDirectory=%s
RuntimeDirectoryMode=0700

[Install]
WantedBy=multi-user.target
`, ghostlinkd, configPath, filepath.Base(controlDir))
	if err := os.WriteFile(unitPath, []byte(unitBody), 0o644); err != nil {
		t.Fatal(err)
	}
	crudSmokeSystemctl(t, "daemon-reload")

	controller := ghostlink.NewSmokeController(ghostlink.SmokeControllerConfig{
		UnitName:          unit,
		SystemctlPath:     ghostlink.DefaultSystemctlBinary,
		BinaryPath:        ghostlinkd,
		ConfigPath:        configPath,
		StateDBPath:       stateDBPath,
		ControlSocketPath: controlSocket,
		OperationPath:     operationPath,
		PeerUID:           uint32(os.Geteuid()),
		ServerUID:         uint32(os.Geteuid()),
	})
	previousManager := runtime.GetManager()
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{
		APIPort:   func() int { return 0 },
		GhostLink: controller,
	}))
	t.Cleanup(func() {
		runtime.SetManager(previousManager)
		_ = exec.Command(ghostlink.DefaultSystemctlBinary, "disable", "--now", unit).Run()
		_ = os.Remove(unitPath)
		_ = os.RemoveAll(controlDir)
		_ = exec.Command(ghostlink.DefaultSystemctlBinary, "daemon-reload").Run()
	})

	settings := ghostlink.Settings{
		Version:         ghostlink.SchemaVersion,
		CertificateFile: certPath,
		PrivateKeyFile:  keyPath,
		ServerName:      "ghostlink.example",
		Clients: []model.Client{
			{Email: "alice@example.test", ID: "alice-device", Secret: crudSmokeAliceSecret, Enable: true},
			{Email: "bob@example.test", ID: "bob-device", Secret: crudSmokeBobSecret, Enable: true},
		},
	}
	settingsRaw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	inboundService := &InboundService{}
	inbound, _, err := inboundService.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     port,
		Protocol: model.GhostLink,
		Settings: string(settingsRaw),
	})
	if err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	initialStatus := crudSmokeWaitReady(t, inboundService)
	initialPID := initialStatus.MainPID
	if initialPID == 0 {
		t.Fatal("initial MainPID is zero")
	}
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "alice-device", crudSmokeAliceSecret, true)
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "bob-device", crudSmokeBobSecret, true)

	clientService := &ClientService{}
	charlie := model.Client{
		Email: "charlie@example.test", ID: "charlie-device",
		Secret: crudSmokeCharlieSecret, Enable: true,
	}
	if _, err := clientService.CreateOne(inboundService, inbound.Id, charlie); err != nil {
		t.Fatalf("single client add: %v", err)
	}
	afterAdd := crudSmokeWaitReady(t, inboundService)
	if afterAdd.MainPID != initialPID {
		t.Fatalf("client add changed MainPID %d -> %d", initialPID, afterAdd.MainPID)
	}
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "charlie-device", crudSmokeCharlieSecret, true)

	alice := model.Client{
		Email: "alice@example.test", ID: "alice-device",
		Secret: crudSmokeAliceNewSecret, Enable: true,
	}
	if _, err := clientService.UpdateByEmail(inboundService, alice.Email, alice, 0); err != nil {
		t.Fatalf("rotate alice: %v", err)
	}
	afterRotate := crudSmokeWaitReady(t, inboundService)
	if afterRotate.MainPID != initialPID {
		t.Fatalf("secret rotation changed MainPID %d -> %d", initialPID, afterRotate.MainPID)
	}
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "alice-device", crudSmokeAliceSecret, false)
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "alice-device", crudSmokeAliceNewSecret, true)

	hold := crudSmokeStartHeldConnections(
		t, probe, certPath, inbound, echo.Addr().String(),
		"bob-device", crudSmokeBobSecret,
	)
	if changed, _, err := clientService.SetClientEnableByEmail(inboundService, "bob@example.test", false); err != nil || !changed {
		t.Fatalf("disable bob changed=%v err=%v", changed, err)
	}
	crudSmokeWaitHeldClosed(t, hold)
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "bob-device", crudSmokeBobSecret, false)
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "charlie-device", crudSmokeCharlieSecret, true)

	if _, err := clientService.DeleteByEmail(inboundService, "charlie@example.test", true); err != nil {
		t.Fatalf("delete charlie: %v", err)
	}
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "charlie-device", crudSmokeCharlieSecret, false)

	if changed, _, err := clientService.SetClientEnableByEmail(inboundService, "alice@example.test", false); err != nil || !changed {
		t.Fatalf("disable last enabled user changed=%v err=%v", changed, err)
	}
	stopped := crudSmokeWaitStopped(t, inboundService)
	if stopped.Running || stopped.MainPID != 0 {
		t.Fatalf("last-user stop status = %#v", stopped)
	}
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "alice-device", crudSmokeAliceNewSecret, false)

	if changed, _, err := clientService.SetClientEnableByEmail(inboundService, "alice@example.test", true); err != nil || !changed {
		t.Fatalf("re-enable alice changed=%v err=%v", changed, err)
	}
	restarted := crudSmokeWaitReady(t, inboundService)
	if restarted.MainPID == 0 {
		t.Fatal("re-enabled service has zero MainPID")
	}
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "alice-device", crudSmokeAliceNewSecret, true)

	crudSmokeSystemctl(t, "restart", unit)
	afterCoreRestart := crudSmokeWaitReady(t, inboundService)
	if afterCoreRestart.MainPID == restarted.MainPID {
		t.Fatal("explicit ghostlinkd restart did not change MainPID")
	}
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "alice-device", crudSmokeAliceNewSecret, true)

	panelController := ghostlink.NewSmokeController(ghostlink.SmokeControllerConfig{
		UnitName:          unit,
		SystemctlPath:     ghostlink.DefaultSystemctlBinary,
		BinaryPath:        ghostlinkd,
		ConfigPath:        configPath,
		StateDBPath:       stateDBPath,
		ControlSocketPath: controlSocket,
		OperationPath:     operationPath,
		PeerUID:           uint32(os.Geteuid()),
		ServerUID:         uint32(os.Geteuid()),
	})
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{GhostLink: panelController}))
	if _, err := (&InboundService{}).ReconcileGhostLink(context.Background()); err != nil {
		t.Fatalf("panel-start reconcile: %v", err)
	}
	panelRestartStatus, err := (&InboundService{}).GhostLinkLifecycleStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if panelRestartStatus.MainPID != afterCoreRestart.MainPID {
		t.Fatalf("panel restart restarted daemon: %d -> %d", afterCoreRestart.MainPID, panelRestartStatus.MainPID)
	}
	crudSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), "alice-device", crudSmokeAliceNewSecret, true)
}

type crudSmokeHold struct {
	cmd    *exec.Cmd
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func crudSmokeStartHeldConnections(
	t *testing.T,
	binary string,
	caPath string,
	inbound *model.Inbound,
	target string,
	clientID string,
	secret string,
) *crudSmokeHold {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"h2_address":  net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)),
		"h3_address":  net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)),
		"server_name": "ghostlink.example",
		"ca_file":     caPath,
		"target":      target,
		"client_id":   clientID,
		"secret":      secret,
		"hold":        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Stdin = bytes.NewReader(payload)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"ready":true`) {
		_ = cmd.Process.Kill()
		t.Fatalf("hold probe did not become ready: line=%q err=%v stderr=%s", line, err, stderr.String())
	}
	return &crudSmokeHold{cmd: cmd, stdout: reader, stderr: stderr}
}

func crudSmokeWaitHeldClosed(t *testing.T, hold *crudSmokeHold) {
	t.Helper()
	type lineResult struct {
		line string
		err  error
	}
	lineDone := make(chan lineResult, 1)
	go func() {
		line, err := hold.stdout.ReadString('\n')
		lineDone <- lineResult{line: line, err: err}
	}()
	select {
	case result := <-lineDone:
		if result.err != nil || !strings.Contains(result.line, `"closed":true`) {
			_ = hold.cmd.Process.Kill()
			t.Fatalf("hold probe did not report both H2/H3 closed: line=%q err=%v stderr=%s", result.line, result.err, hold.stderr.String())
		}
	case <-time.After(8 * time.Second):
		_ = hold.cmd.Process.Kill()
		t.Fatalf("held H2/H3 sessions were not revoked in time: %s", hold.stderr.String())
	}
	done := make(chan error, 1)
	go func() { done <- hold.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hold probe failed after revocation: %v: %s", err, hold.stderr.String())
		}
	case <-time.After(8 * time.Second):
		_ = hold.cmd.Process.Kill()
		t.Fatalf("hold probe did not exit after reporting closure: %s", hold.stderr.String())
	}
}

func crudSmokeWaitReady(t *testing.T, inboundService *InboundService) ghostlink.Status {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	var last ghostlink.Status
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = inboundService.GhostLinkLifecycleStatus(context.Background())
		if lastErr == nil && last.Ready {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GhostLink did not become ready: status=%#v err=%v", last, lastErr)
	return ghostlink.Status{}
}

func crudSmokeWaitStopped(t *testing.T, inboundService *InboundService) ghostlink.Status {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last ghostlink.Status
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = inboundService.GhostLinkLifecycleStatus(context.Background())
		if lastErr == nil && !last.Running {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GhostLink did not stop: status=%#v err=%v", last, lastErr)
	return ghostlink.Status{}
}

func crudSmokeProbe(
	t *testing.T,
	binary string,
	caPath string,
	inbound *model.Inbound,
	target string,
	clientID string,
	secret string,
	wantSuccess bool,
) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"h2_address":  net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)),
		"h3_address":  net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)),
		"server_name": "ghostlink.example",
		"ca_file":     caPath,
		"target":      target,
		"client_id":   clientID,
		"secret":      secret,
		"payload":     "round4-service-probe",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Stdin = bytes.NewReader(payload)
	output, err := cmd.CombinedOutput()
	if wantSuccess {
		if err != nil {
			t.Fatalf("probe %s unexpectedly failed: %v: %s", clientID, err, output)
		}
		var result struct {
			H2 bool `json:"h2"`
			H3 bool `json:"h3"`
		}
		if err := json.Unmarshal(output, &result); err != nil || !result.H2 || !result.H3 {
			t.Fatalf("probe %s result=%s err=%v", clientID, output, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("probe %s unexpectedly succeeded: %s", clientID, output)
	}
}

func crudSmokeSystemctl(t *testing.T, args ...string) {
	t.Helper()
	output, err := exec.Command(ghostlink.DefaultSystemctlBinary, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func crudSmokeDualPort(t *testing.T) int {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcp.Addr().(*net.TCPAddr).Port
	_ = tcp.Close()
	udp, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	_ = udp.Close()
	return port
}

func crudSmokeEcho(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						_, _ = conn.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}
