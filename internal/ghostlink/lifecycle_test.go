package ghostlink

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
)

type fakeServiceManager struct {
	mu             sync.Mutex
	snapshot       unitSnapshot
	available      bool
	enableCalls    int
	disableCalls   int
	restartCalls   int
	restartFailure int
	enableErr      error
	disableErr     error
	leaveInactive  bool
}

func (m *fakeServiceManager) Available() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.available
}

func (m *fakeServiceManager) Snapshot(context.Context) (unitSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot, nil
}

func (m *fakeServiceManager) EnableNow(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enableCalls++
	if m.enableErr != nil {
		return m.enableErr
	}
	if m.leaveInactive {
		m.snapshot = unitSnapshot{Loaded: true, Active: false}
		return nil
	}
	m.snapshot = unitSnapshot{Loaded: true, Active: true, PID: 32001 + m.enableCalls}
	return nil
}

func (m *fakeServiceManager) DisableNow(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disableCalls++
	if m.disableErr != nil {
		return m.disableErr
	}
	m.snapshot = unitSnapshot{Loaded: true, Active: false}
	return nil
}

func (m *fakeServiceManager) Restart(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restartCalls++
	if m.restartFailure > 0 {
		m.restartFailure--
		m.snapshot = unitSnapshot{Loaded: true, Active: false}
		return errors.New("injected restart failure")
	}
	m.snapshot = unitSnapshot{Loaded: true, Active: true, PID: 42001 + m.restartCalls}
	return nil
}

func (m *fakeServiceManager) counts() (enable, disable, restart int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enableCalls, m.disableCalls, m.restartCalls
}

type fakeControlClient struct {
	mu         sync.Mutex
	statusFunc func() controlResponse
	err        error
}

func (c *fakeControlClient) Status(context.Context) (controlResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return controlResponse{}, c.err
	}
	if c.statusFunc == nil {
		return controlResponse{}, errors.New("fake control status is not configured")
	}
	return c.statusFunc(), nil
}

func (c *fakeControlClient) Usage(context.Context) (controlResponse, error) {
	return controlResponse{}, errors.New("fake control usage is not implemented")
}

func (c *fakeControlClient) ApplyUsers(
	context.Context,
	string,
	string,
	map[string]RuntimeUser,
) (controlResponse, error) {
	return controlResponse{}, errors.New("fake control apply is not implemented")
}

type lifecycleFixture struct {
	inbound *model.Inbound
	cfg     controllerConfig
	manager *fakeServiceManager
	ctl     *systemdController
}

func newLifecycleFixture(t *testing.T) lifecycleFixture {
	t.Helper()
	root := t.TempDir()
	binary := filepath.Join(root, "ghostlinkd")
	if err := os.WriteFile(binary, []byte("test binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := controllerConfig{
		unitName:          DefaultSystemdUnit,
		binaryPath:        binary,
		configPath:        filepath.Join(root, "config", "server.json"),
		stateDBPath:       filepath.Join(root, "state", "state.db"),
		startWait:         250 * time.Millisecond,
		pollInterval:      5 * time.Millisecond,
		preflightPorts:    true,
		controlSocket:     "/run/ghostlinkd-3xui-smoke-test/control.sock",
		controlPeerUID:    1000,
		controlServerUID:  1000,
		allowTestPaths:    true,
		userOperationPath: filepath.Join(root, "config", "pending-user-op.json"),
	}
	manager := &fakeServiceManager{snapshot: unitSnapshot{Loaded: true}, available: true}
	inbound := &model.Inbound{
		Id:       1,
		Protocol: model.GhostLink,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38443,
		Settings: marshalSettings(t, validSettings(t)),
	}
	state, err := BuildDesiredStateWithControl(inbound, cfg.stateDBPath, cfg.controlPeerUID, cfg.controlSocket)
	if err != nil {
		t.Fatal(err)
	}
	control := &fakeControlClient{}
	control.statusFunc = func() controlResponse {
		manager.mu.Lock()
		snapshot := manager.snapshot
		manager.mu.Unlock()
		return controlResponse{
			OK:           true,
			PID:          snapshot.PID,
			PeerPID:      snapshot.PID,
			PeerUID:      cfg.controlServerUID,
			InstanceID:   "test-instance",
			Serving:      snapshot.Active,
			H2Listen:     state.Config.Listen,
			H3Listen:     state.Config.Listen,
			ActiveUsers:  state.EnabledUsers,
			UserRevision: state.UserRevision,
		}
	}
	cfg.controlClient = control
	return lifecycleFixture{
		inbound: inbound,
		cfg:     cfg,
		manager: manager,
		ctl:     newSystemdController(cfg, manager),
	}
}

func TestSystemdControllerApplyIsIdempotentAndProtectsConfig(t *testing.T) {
	f := newLifecycleFixture(t)
	status, err := f.ctl.Reconcile(context.Background(), f.inbound)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !status.Running || !status.Ready || status.Reason != "running" || status.MainPID == 0 {
		t.Fatalf("first status = %#v", status)
	}
	if enable, disable, restart := f.manager.counts(); enable != 1 || disable != 0 || restart != 0 {
		t.Fatalf("first commands enable=%d disable=%d restart=%d", enable, disable, restart)
	}
	configInfo, err := os.Stat(f.cfg.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && configInfo.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", configInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(f.cfg.configPath))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("config dir mode = %o, want 700", dirInfo.Mode().Perm())
	}
	configData, err := os.ReadFile(f.cfg.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configData), testSecret) {
		t.Fatal("protected config did not contain the runtime secret")
	}
	statusJSON, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(statusJSON), testSecret) || strings.Contains(string(statusJSON), "alice-device") {
		t.Fatalf("status leaked a credential: %s", statusJSON)
	}

	// A panel restart constructs a new controller. The active unit and exact
	// config/revision must be adopted without a restart.
	restartedController := newSystemdController(f.cfg, f.manager)
	if _, err := restartedController.Reconcile(context.Background(), f.inbound); err != nil {
		t.Fatalf("reconcile after panel restart: %v", err)
	}
	if enable, disable, restart := f.manager.counts(); enable != 1 || disable != 0 || restart != 0 {
		t.Fatalf("idempotent commands enable=%d disable=%d restart=%d", enable, disable, restart)
	}
}

func TestSystemdControllerSupportsH2OnlyPresets(t *testing.T) {
	for _, tc := range []struct {
		preset  string
		capsule bool
	}{
		{preset: PresetH2},
		{preset: PresetH2Capsule, capsule: true},
	} {
		t.Run(tc.preset, func(t *testing.T) {
			f := newLifecycleFixture(t)
			settings := validSettings(t)
			settings.TransportPreset = tc.preset
			settings.IdentityTLS = testIdentityTLS(t)
			settings.CertificateFile = ""
			settings.PrivateKeyFile = ""
			f.inbound.Settings = marshalSettings(t, settings)
			state, err := BuildDesiredStateWithControl(
				f.inbound, f.cfg.stateDBPath, f.cfg.controlPeerUID, f.cfg.controlSocket,
			)
			if err != nil {
				t.Fatal(err)
			}
			control := f.cfg.controlClient.(*fakeControlClient)
			control.statusFunc = func() controlResponse {
				f.manager.mu.Lock()
				snapshot := f.manager.snapshot
				f.manager.mu.Unlock()
				return controlResponse{
					OK: true, PID: snapshot.PID, PeerPID: snapshot.PID, PeerUID: f.cfg.controlServerUID,
					InstanceID: "test-h2-instance", Serving: snapshot.Active,
					H2Listen: state.Config.Listen, ActiveUsers: state.EnabledUsers,
					UserRevision: state.UserRevision,
				}
			}
			status, err := f.ctl.Reconcile(context.Background(), f.inbound)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if !status.Ready || status.Reason != "running" {
				t.Fatalf("H2 lifecycle status = %#v", status)
			}
			configData, err := os.ReadFile(f.cfg.configPath)
			if err != nil {
				t.Fatal(err)
			}
			var config ManagementConfig
			if err := json.Unmarshal(configData, &config); err != nil {
				t.Fatal(err)
			}
			if config.Transport != TransportH2 || config.EnableH2UDPCapsule != tc.capsule ||
				config.IdentityTLS == nil || config.TLSCertFile != "" || config.TLSKeyFile != "" {
				t.Fatalf("H2 management config = %#v", config)
			}
		})
	}
}

func TestSystemdControllerStopDisablesAndRemovesProtectedConfig(t *testing.T) {
	f := newLifecycleFixture(t)
	if _, err := f.ctl.Reconcile(context.Background(), f.inbound); err != nil {
		t.Fatal(err)
	}
	status, err := f.ctl.Stop(context.Background())
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if status.Running || status.Reason != "stopped" {
		t.Fatalf("stop status = %#v", status)
	}
	if fileExists(f.cfg.configPath) || fileExists(f.ctl.revisionPath()) {
		t.Fatal("protected config or revision remained after stop")
	}
	if _, disable, _ := f.manager.counts(); disable != 1 {
		t.Fatalf("disable calls = %d, want 1", disable)
	}
}

func TestSystemdControllerDoesNotStartWithoutEnabledUsers(t *testing.T) {
	f := newLifecycleFixture(t)
	settings := validSettings(t)
	settings.Clients[0].Enable = false
	f.inbound.Settings = marshalSettings(t, settings)

	status, err := f.ctl.Reconcile(context.Background(), f.inbound)
	if err == nil || !strings.Contains(err.Error(), "without at least one enabled user") {
		t.Fatalf("empty user reconcile error = %v", err)
	}
	if status.Running || status.Reason != "waiting_for_enabled_users" {
		t.Fatalf("status = %#v", status)
	}
	if enable, _, restart := f.manager.counts(); enable != 0 || restart != 0 {
		t.Fatalf("empty user config started the unit: enable=%d restart=%d", enable, restart)
	}
	if fileExists(f.cfg.configPath) {
		t.Fatal("empty user config wrote a protected config")
	}
}

func TestSystemdControllerRejectsRealPortCollision(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	f := newLifecycleFixture(t)
	f.inbound.Port = listener.Addr().(*net.TCPAddr).Port

	status, err := f.ctl.Reconcile(context.Background(), f.inbound)
	if err == nil || !strings.Contains(err.Error(), "TCP/H2 port") {
		t.Fatalf("port collision error = %v", err)
	}
	if status.Reason != "port_unavailable" || status.Running {
		t.Fatalf("port collision status = %#v", status)
	}
	if enable, _, restart := f.manager.counts(); enable != 0 || restart != 0 {
		t.Fatalf("port collision reached systemd: enable=%d restart=%d", enable, restart)
	}
	if fileExists(f.cfg.configPath) {
		t.Fatal("port collision wrote a protected config")
	}
}

func TestSystemdControllerRejectsRealUDPPortCollision(t *testing.T) {
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	f := newLifecycleFixture(t)
	f.inbound.Port = packet.LocalAddr().(*net.UDPAddr).Port

	status, err := f.ctl.Reconcile(context.Background(), f.inbound)
	if err == nil || (runtime.GOOS != "windows" && !strings.Contains(err.Error(), "UDP/H3 port")) {
		t.Fatalf("UDP port collision error = %v", err)
	}
	if status.Reason != "port_unavailable" || status.Running {
		t.Fatalf("UDP port collision status = %#v", status)
	}
}

func TestSystemdControllerRestoresPreviousConfigAfterRestartFailure(t *testing.T) {
	f := newLifecycleFixture(t)
	first, err := f.ctl.Reconcile(context.Background(), f.inbound)
	if err != nil {
		t.Fatal(err)
	}
	firstConfig, err := os.ReadFile(f.cfg.configPath)
	if err != nil {
		t.Fatal(err)
	}

	f.manager.mu.Lock()
	f.manager.restartFailure = 1
	f.manager.mu.Unlock()
	changed := *f.inbound
	settings := validSettings(t)
	settings.Clients[0].Secret = "ghostlink-second-valid-secret-at-least-32-bytes"
	changed.Settings = marshalSettings(t, settings)

	status, err := f.ctl.Reconcile(context.Background(), &changed)
	if err == nil || !strings.Contains(err.Error(), "injected restart failure") {
		t.Fatalf("restart failure error = %v", err)
	}
	if status.Running || status.Reason != "start_failed" {
		t.Fatalf("restart failure status = %#v", status)
	}
	restored, err := os.ReadFile(f.cfg.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(firstConfig) {
		t.Fatal("controller did not restore the previous protected config")
	}
	activeStatus, err := f.ctl.Status(context.Background(), f.inbound)
	if err != nil {
		t.Fatal(err)
	}
	if activeStatus.ActiveRevision != first.DesiredRevision {
		t.Fatalf("restored active revision = %q, want %q", activeStatus.ActiveRevision, first.DesiredRevision)
	}
	if _, _, restart := f.manager.counts(); restart != 2 {
		t.Fatalf("restart calls = %d, want failed attempt plus rollback", restart)
	}
}

func TestSystemdControllerInitialStartFailureRemovesConfig(t *testing.T) {
	f := newLifecycleFixture(t)
	f.manager.enableErr = errors.New("injected enable failure")
	status, err := f.ctl.Reconcile(context.Background(), f.inbound)
	if err == nil || !strings.Contains(err.Error(), "injected enable failure") {
		t.Fatalf("initial start failure = %v", err)
	}
	if status.Running || status.Ready || status.Reason != "start_failed" {
		t.Fatalf("initial failure status = %#v", status)
	}
	if fileExists(f.cfg.configPath) || fileExists(f.ctl.revisionPath()) {
		t.Fatal("initial start failure left protected config behind")
	}
	if _, disable, _ := f.manager.counts(); disable != 1 {
		t.Fatalf("rollback disable calls = %d, want 1", disable)
	}
}

func TestSystemdControllerProcessExitDuringStartupFailsReadyWait(t *testing.T) {
	f := newLifecycleFixture(t)
	f.cfg.startWait = 40 * time.Millisecond
	f.cfg.pollInterval = 5 * time.Millisecond
	f.ctl = newSystemdController(f.cfg, f.manager)
	f.manager.leaveInactive = true
	status, err := f.ctl.Reconcile(context.Background(), f.inbound)
	if err == nil || !strings.Contains(err.Error(), "did not become active and listen") {
		t.Fatalf("startup exit error = %v", err)
	}
	if status.Running || status.Ready || status.Reason != "start_failed" {
		t.Fatalf("startup exit status = %#v", status)
	}
	if fileExists(f.cfg.configPath) {
		t.Fatal("startup exit left protected config behind")
	}
}

func TestSystemdControllerRejectsMissingUnit(t *testing.T) {
	f := newLifecycleFixture(t)
	f.manager.snapshot = unitSnapshot{Loaded: false}
	_, err := f.ctl.Reconcile(context.Background(), f.inbound)
	if err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Fatalf("missing unit error = %v", err)
	}
	if fileExists(f.cfg.configPath) {
		t.Fatal("missing systemd unit wrote a protected config")
	}
}

func TestSystemdControllerDoesNotTrustPortOwnerWithoutPIDMatch(t *testing.T) {
	f := newLifecycleFixture(t)
	f.manager.mu.Lock()
	f.manager.snapshot = unitSnapshot{Loaded: true, Active: true, PID: 9001}
	f.manager.mu.Unlock()
	baseStatus := f.cfg.controlClient.(*fakeControlClient)
	baseStatus.mu.Lock()
	baseStatus.statusFunc = func() controlResponse {
		return controlResponse{
			OK: true, PID: 9002, PeerPID: 9002, PeerUID: f.cfg.controlServerUID,
			InstanceID: "wrong-process", Serving: true,
			H2Listen: "127.0.0.1:38443", H3Listen: "127.0.0.1:38443",
			ActiveUsers: 1, UserRevision: "sha256:test",
		}
	}
	baseStatus.mu.Unlock()
	status, err := f.ctl.Status(context.Background(), f.inbound)
	if err != nil {
		t.Fatal(err)
	}
	if status.Ready || status.RuntimeReady || status.Reason != "runtime_identity_mismatch" {
		t.Fatalf("identity mismatch status = %#v", status)
	}
}

func TestSystemdControllerStopsActiveFixedUnitWithMissingConfig(t *testing.T) {
	f := newLifecycleFixture(t)
	f.manager.mu.Lock()
	f.manager.snapshot = unitSnapshot{Loaded: true, Active: true, PID: 9100}
	f.manager.mu.Unlock()
	if fileExists(f.cfg.configPath) || fileExists(f.ctl.revisionPath()) {
		t.Fatal("test requires missing config and revision")
	}
	status, err := f.ctl.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Running || status.Reason != "stopped" {
		t.Fatalf("orphan stop status = %#v", status)
	}
	if _, disable, _ := f.manager.counts(); disable != 1 {
		t.Fatalf("disable calls = %d, want 1", disable)
	}
}

func TestSystemdControllerMissingConfigWithoutSystemdIsNoOp(t *testing.T) {
	f := newLifecycleFixture(t)
	f.manager.mu.Lock()
	f.manager.available = false
	f.manager.snapshot = unitSnapshot{}
	f.manager.mu.Unlock()
	status, err := f.ctl.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Running || status.Reason != "stopped" {
		t.Fatalf("no-systemd stop status = %#v", status)
	}
	if _, disable, _ := f.manager.counts(); disable != 0 {
		t.Fatalf("disable calls = %d, want 0", disable)
	}
}

func TestListenerMatchesDualStackWildcard(t *testing.T) {
	cases := []struct {
		actual   string
		desired  string
		expected bool
	}{
		{"[::]:28443", "0.0.0.0:28443", true},
		{"0.0.0.0:28443", "[::]:28443", true},
		{":28443", "0.0.0.0:28443", true},
		{"127.0.0.1:28443", "127.0.0.1:28443", true},
		{"[::]:28443", "127.0.0.1:28443", false},
		{"127.0.0.1:28443", "0.0.0.0:28443", false},
		{"0.0.0.0:28443", "0.0.0.0:8080", false},
	}
	for _, tc := range cases {
		if got := listenerMatches(tc.actual, tc.desired); got != tc.expected {
			t.Errorf("listenerMatches(%q, %q) = %v, want %v", tc.actual, tc.desired, got, tc.expected)
		}
	}
}
