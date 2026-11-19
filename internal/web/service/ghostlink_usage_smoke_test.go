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
	"github.com/mhsanaei/3x-ui/v3/internal/xray"
)

const usageSmokeSecret = "ghostlink-usage-smoke-alice-secret-000001"

func TestGhostLinkServiceLiveUsageAccountingSmoke(t *testing.T) {
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
	controlDir := fmt.Sprintf("/run/ghostlinkd-3xui-smoke-usage-%d", os.Getpid())
	controlSocket := filepath.Join(controlDir, "control.sock")
	unit := fmt.Sprintf("ghostlinkd-3xui-smoke-usage-%d.service", os.Getpid())
	unitPath := filepath.Join("/run/systemd/system", unit)
	unitBody := fmt.Sprintf(`[Unit]
Description=GhostLink 3x-ui usage smoke

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
		Clients: []model.Client{{
			Email: "usage-alice@example.test", ID: "usage-alice-device",
			Secret: usageSmokeSecret, Enable: true,
		}},
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
	ready := crudSmokeWaitReady(t, inboundService)
	initialPID := ready.MainPID

	if err := database.GetDB().Create(&xray.ClientTraffic{
		InboundId: -999,
		Email:     "xray-unrelated@example.test",
		Enable:    true,
		Up:        11,
		Down:      22,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := inboundService.CollectGhostLinkUsage(context.Background()); err != nil {
		t.Fatalf("baseline collection: %v", err)
	}

	firstPayload := strings.Repeat("a", 8192)
	usageSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), firstPayload)
	result, err := inboundService.CollectGhostLinkUsage(context.Background())
	if err != nil {
		t.Fatalf("first usage collection: %v", err)
	}
	if result.UploadDelta != 2*uint64(len(firstPayload)) || result.DownloadDelta != 2*uint64(len(firstPayload)) {
		t.Fatalf("H2+H3 first deltas = up %d down %d", result.UploadDelta, result.DownloadDelta)
	}
	assertUsageSmokeTotals(t, inbound.Id, 2*uint64(len(firstPayload)), 2*uint64(len(firstPayload)))

	result, err = inboundService.CollectGhostLinkUsage(context.Background())
	if err != nil {
		t.Fatalf("duplicate collection: %v", err)
	}
	if result.UploadDelta != 0 || result.DownloadDelta != 0 {
		t.Fatalf("duplicate collection charged again: %#v", result)
	}
	assertUsageSmokeTotals(t, inbound.Id, 2*uint64(len(firstPayload)), 2*uint64(len(firstPayload)))

	crudSmokeSystemctl(t, "restart", unit)
	restarted := crudSmokeWaitReady(t, inboundService)
	if restarted.MainPID == initialPID {
		t.Fatal("explicit GhostLink restart did not change MainPID")
	}
	secondPayload := strings.Repeat("b", 4096)
	usageSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), secondPayload)
	if _, err := inboundService.CollectGhostLinkUsage(context.Background()); err != nil {
		t.Fatalf("post-restart usage collection: %v", err)
	}
	assertUsageSmokeTotals(
		t,
		inbound.Id,
		2*uint64(len(firstPayload)+len(secondPayload)),
		2*uint64(len(firstPayload)+len(secondPayload)),
	)

	finalPayload := strings.Repeat("c", 1024)
	usageSmokeProbe(t, probe, certPath, inbound, echo.Addr().String(), finalPayload)
	if changed, _, err := (&ClientService{}).SetClientEnableByEmail(inboundService, "usage-alice@example.test", false); err != nil || !changed {
		t.Fatalf("disable last user changed=%v err=%v", changed, err)
	}
	crudSmokeWaitStopped(t, inboundService)
	if _, err := inboundService.CollectGhostLinkUsage(context.Background()); err != nil {
		t.Fatalf("post-stop usage collection: %v", err)
	}
	assertUsageSmokeTotals(
		t,
		inbound.Id,
		2*uint64(len(firstPayload)+len(secondPayload)+len(finalPayload)),
		2*uint64(len(firstPayload)+len(secondPayload)+len(finalPayload)),
	)

	var unrelated xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", "xray-unrelated@example.test").First(&unrelated).Error; err != nil {
		t.Fatal(err)
	}
	if unrelated.Up != 11 || unrelated.Down != 22 {
		t.Fatalf("unrelated Xray ledger changed: %+v", unrelated)
	}
}

func usageSmokeProbe(
	t *testing.T,
	binary string,
	caPath string,
	inbound *model.Inbound,
	target string,
	payloadText string,
) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"h2_address":  net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)),
		"h3_address":  net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port)),
		"server_name": "ghostlink.example",
		"ca_file":     caPath,
		"target":      target,
		"client_id":   "usage-alice-device",
		"secret":      usageSmokeSecret,
		"payload":     payloadText,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Stdin = bytes.NewReader(payload)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("usage probe failed: %v: %s", err, output)
	}
	var result struct {
		H2 bool `json:"h2"`
		H3 bool `json:"h3"`
	}
	if err := json.Unmarshal(output, &result); err != nil || !result.H2 || !result.H3 {
		t.Fatalf("usage probe result=%s err=%v", output, err)
	}
}

func assertUsageSmokeTotals(t *testing.T, inboundID int, wantUp, wantDown uint64) {
	t.Helper()
	db := database.GetDB()
	var client xray.ClientTraffic
	if err := db.Where("email = ?", "usage-alice@example.test").First(&client).Error; err != nil {
		t.Fatal(err)
	}
	var inbound model.Inbound
	if err := db.First(&inbound, inboundID).Error; err != nil {
		t.Fatal(err)
	}
	if uint64(client.Up) != wantUp || uint64(client.Down) != wantDown ||
		uint64(inbound.Up) != wantUp || uint64(inbound.Down) != wantDown {
		t.Fatalf(
			"usage totals client=(%d,%d) inbound=(%d,%d), want=(%d,%d)",
			client.Up, client.Down, inbound.Up, inbound.Down, wantUp, wantDown,
		)
	}
	t.Logf("usage totals verified: client/inbound up=%d down=%d", wantUp, wantDown)
}
