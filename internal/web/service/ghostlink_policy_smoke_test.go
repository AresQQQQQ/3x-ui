//go:build linux && ghostlink_crud_smoke

package service

import (
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

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/web/runtime"
)

const expiredBobSecret = "ghostlink-expired-bob-secret-0000000007"

func TestGhostLinkServiceLiveQuotaExpiryResetSmoke(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to install an isolated systemd unit")
	}
	ghostlinkd := strings.TrimSpace(os.Getenv("GHOSTLINKD_SMOKE_BINARY"))
	probe := strings.TrimSpace(os.Getenv("GHOSTLINK_USER_PROBE_BINARY"))
	if ghostlinkd == "" || probe == "" {
		t.Skip("GHOSTLINKD_SMOKE_BINARY and GHOSTLINK_USER_PROBE_BINARY are required")
	}

	setupConflictDB(t)
	root := t.TempDir()
	certPath, keyPath := ghostLinkTestCertificate(t)
	port := crudSmokeDualPort(t)
	echo := crudSmokeEcho(t)
	configPath := filepath.Join(root, "config", "server.json")
	stateDBPath := filepath.Join(root, "state", "state.db")
	operationPath := filepath.Join(root, "config", "pending-user-op.json")
	controlDir := fmt.Sprintf("/run/ghostlinkd-3xui-smoke-policy-%d", os.Getpid())
	controlSocket := filepath.Join(controlDir, "control.sock")
	unit := fmt.Sprintf("ghostlinkd-3xui-smoke-policy-%d.service", os.Getpid())
	unitPath := filepath.Join("/run/systemd/system", unit)
	unitBody := fmt.Sprintf(`[Unit]
Description=GhostLink 3x-ui policy smoke

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
			{
				Email: "usage-alice@example.test", ID: "usage-alice-device",
				Secret: usageSmokeSecret, Enable: true, TotalGB: 16 * 1024,
			},
			{
				Email: "expired-bob@example.test", ID: "expired-bob-device",
				Secret: expiredBobSecret, Enable: true,
				ExpiryTime: time.Now().Add(-time.Hour).UnixMilli(),
			},
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
	firstPID := crudSmokeWaitReady(t, inboundService).MainPID
	usageSmokeProbeExpectFailure(
		t, probe, certPath, inbound, echo.Addr().String(),
		"expired-bob-device", expiredBobSecret, "policy-reject", "expired-first-start",
	)

	usageSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), strings.Repeat("q", 2048))
	usageSmokeProbeExpectFailure(
		t, probe, certPath, inbound, echo.Addr().String(),
		"usage-alice-device", usageSmokeSecret, strings.Repeat("x", 8192), "quota-exhausted",
	)
	if _, err := inboundService.CollectGhostLinkUsage(context.Background()); err != nil {
		t.Fatalf("quota collection: %v", err)
	}
	crudSmokeWaitStopped(t, inboundService)
	var watermark model.GhostLinkUsageWatermark
	if err := database.GetDB().Where("client_id = ?", "usage-alice-device").First(&watermark).Error; err != nil {
		t.Fatal(err)
	}
	if watermark.PolicyState != ghostLinkPolicyQuotaReached || watermark.EffectiveEnabled {
		t.Fatalf("quota policy = %+v", watermark)
	}
	if firstPID == 0 {
		t.Fatal("initial service PID is zero")
	}

	if _, err := (&ClientService{}).ResetTrafficByEmail(inboundService, "usage-alice@example.test"); err != nil {
		t.Fatalf("manual reset: %v", err)
	}
	afterReset := crudSmokeWaitReady(t, inboundService)
	if afterReset.MainPID == 0 {
		t.Fatal("manual reset did not restart service")
	}
	usageSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), strings.Repeat("r", 2048))
	if _, err := inboundService.CollectGhostLinkUsage(context.Background()); err != nil {
		t.Fatalf("post-reset collection: %v", err)
	}
	var traffic struct {
		Up   int64
		Down int64
	}
	if err := database.GetDB().Table("client_traffics").Where("email = ?", "usage-alice@example.test").First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if traffic.Up != 4096 || traffic.Down != 4096 {
		t.Fatalf("manual reset retained old panel usage: up=%d down=%d", traffic.Up, traffic.Down)
	}
	activePID := afterReset.MainPID
	adjusted := model.Client{
		Email: "usage-alice@example.test", ID: "usage-alice-device",
		Secret: usageSmokeSecret, Enable: true, TotalGB: 32 * 1024,
	}
	if _, err := (&ClientService{}).UpdateByEmail(inboundService, adjusted.Email, adjusted, 0); err != nil {
		t.Fatalf("adjust quota: %v", err)
	}
	afterQuotaChange := crudSmokeWaitReady(t, inboundService)
	if afterQuotaChange.MainPID != activePID {
		t.Fatalf("normal quota update restarted service: %d -> %d", activePID, afterQuotaChange.MainPID)
	}

	expiry := time.Now().Add(2 * time.Second).UnixMilli()
	updated := adjusted
	updated.ExpiryTime = expiry
	if _, err := (&ClientService{}).UpdateByEmail(inboundService, updated.Email, updated, 0); err != nil {
		t.Fatalf("set expiry: %v", err)
	}
	afterExpirySet := crudSmokeWaitReady(t, inboundService)
	if afterExpirySet.MainPID != activePID {
		t.Fatalf("expiry update restarted service: %d -> %d", activePID, afterExpirySet.MainPID)
	}
	time.Sleep(3 * time.Second)
	if _, err := inboundService.CollectGhostLinkUsage(context.Background()); err != nil {
		t.Fatalf("expiry collection: %v", err)
	}
	crudSmokeWaitStopped(t, inboundService)
	usageSmokeProbeExpectFailure(
		t, probe, certPath, inbound, echo.Addr().String(),
		"usage-alice-device", usageSmokeSecret, "policy-reject", "expired",
	)
	crudSmokeSystemctl(t, "start", unit)
	time.Sleep(500 * time.Millisecond)
	usageSmokeProbeExpectFailure(
		t, probe, certPath, inbound, echo.Addr().String(),
		"usage-alice-device", usageSmokeSecret, "policy-reject", "expired-after-core-restart",
	)
	crudSmokeSystemctl(t, "stop", unit)

	updated.ExpiryTime = time.Now().Add(time.Hour).UnixMilli()
	if _, err := (&ClientService{}).UpdateByEmail(inboundService, updated.Email, updated, 0); err != nil {
		t.Fatalf("renew expiry: %v", err)
	}
	crudSmokeWaitReady(t, inboundService)
	usageSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), strings.Repeat("s", 1024))
}

func usageSmokeProbeExpectFailure(
	t *testing.T,
	binary string,
	caPath string,
	inbound *model.Inbound,
	target string,
	clientID string,
	secret string,
	payloadText string,
	reason string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	payload, err := json.Marshal(map[string]any{
		"h2_address":  net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)),
		"h3_address":  net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)),
		"server_name": "ghostlink.example",
		"ca_file":     caPath,
		"target":      target,
		"client_id":   clientID,
		"secret":      secret,
		"payload":     payloadText,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = bytes.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("%s user unexpectedly connected: %s", reason, output)
	}
}
