package ghostlink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
)

const (
	DefaultSystemdUnit        = "ghostlinkd-3xui.service"
	DefaultGhostLinkdBinary   = "/opt/ghostlink-vnext/bin/ghostlinkd"
	DefaultGhostLinkdConfig   = "/etc/ghostlinkd-3xui/server.json"
	DefaultGhostLinkdStateDB  = "/var/lib/ghostlinkd-3xui/state.db"
	DefaultSystemctlBinary    = "/usr/bin/systemctl"
	DefaultControlSocketPath  = "/run/ghostlinkd-3xui/control.sock"
	TestControlSocketRoot     = "/run/ghostlinkd-3xui-smoke-"
	DefaultUserOperationPath  = "/etc/ghostlinkd-3xui/pending-user-op.json"
	ghostLinkRevisionFileName = "server.revision.json"
)

// Controller owns the one fixed GhostLink process desired by a local 3x-ui
// GhostLink inbound. Implementations must never accept a unit name, command, or
// writable path from Inbound settings.
type Controller interface {
	Reconcile(ctx context.Context, inbound *model.Inbound) (Status, error)
	Stop(ctx context.Context) (Status, error)
	Status(ctx context.Context, inbound *model.Inbound) (Status, error)
}

// Status is safe to expose through the authenticated panel API. It deliberately
// contains no ClientID, Secret, certificate path, or command output.
type Status struct {
	Unit                string `json:"unit"`
	Expected            bool   `json:"expected"`
	Running             bool   `json:"running"`
	Ready               bool   `json:"ready"`
	RuntimeReady        bool   `json:"runtimeReady"`
	Listen              string `json:"listen,omitempty"`
	DesiredRevision     string `json:"desiredRevision,omitempty"`
	ActiveRevision      string `json:"activeRevision,omitempty"`
	Reason              string `json:"reason"`
	MainPID             int    `json:"mainPid,omitempty"`
	RuntimePID          int    `json:"runtimePid,omitempty"`
	InstanceID          string `json:"instanceId,omitempty"`
	DesiredUserRevision string `json:"desiredUserRevision,omitempty"`
	RuntimeRevision     string `json:"runtimeRevision,omitempty"`
	PendingOperationID  string `json:"pendingOperationId,omitempty"`
	PendingPhase        string `json:"pendingPhase,omitempty"`
	UsageState          string `json:"usageState,omitempty"`
	UsageErrorCode      string `json:"usageErrorCode,omitempty"`
	UsageLastSnapshotAt int64  `json:"usageLastSnapshotAt,omitempty"`
	PolicyState         string `json:"policyState,omitempty"`
	PolicyErrorCode     string `json:"policyErrorCode,omitempty"`
}

type controllerConfig struct {
	unitName            string
	binaryPath          string
	configPath          string
	stateDBPath         string
	startWait           time.Duration
	pollInterval        time.Duration
	preflightPorts      bool
	controlSocket       string
	controlPeerUID      uint32
	controlServerUID    uint32
	controlClient       localControlClient
	allowTestPaths      bool
	container           bool
	userOperationPath   string
	allowPrivateTargets bool
}

type unitSnapshot struct {
	Loaded bool
	Active bool
	PID    int
}

type serviceManager interface {
	Available() bool
	Snapshot(ctx context.Context) (unitSnapshot, error)
	EnableNow(ctx context.Context) error
	DisableNow(ctx context.Context) error
	Restart(ctx context.Context) error
}

type systemctlManager struct {
	path string
	unit string
}

func (m *systemctlManager) Available() bool {
	info, err := os.Stat(m.path)
	return err == nil && info.Mode().IsRegular()
}

func (m *systemctlManager) command(ctx context.Context, args ...string) ([]byte, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, m.path, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return output, fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), message)
	}
	return output, nil
}

func (m *systemctlManager) Snapshot(ctx context.Context) (unitSnapshot, error) {
	output, err := m.command(ctx,
		"show", m.unit,
		"--property=LoadState",
		"--property=ActiveState",
		"--property=MainPID",
		"--no-pager",
	)
	if err != nil {
		return unitSnapshot{}, err
	}
	properties := make(map[string]string, 3)
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			properties[key] = value
		}
	}
	snapshot := unitSnapshot{
		Loaded: properties["LoadState"] == "loaded",
		Active: properties["ActiveState"] == "active",
	}
	if snapshot.PID, _ = strconv.Atoi(properties["MainPID"]); snapshot.PID < 0 {
		snapshot.PID = 0
	}
	return snapshot, nil
}

func (m *systemctlManager) EnableNow(ctx context.Context) error {
	_, err := m.command(ctx, "enable", "--now", m.unit)
	return err
}

func (m *systemctlManager) DisableNow(ctx context.Context) error {
	_, err := m.command(ctx, "disable", "--now", m.unit)
	return err
}

func (m *systemctlManager) Restart(ctx context.Context) error {
	_, err := m.command(ctx, "restart", m.unit)
	return err
}

type systemdController struct {
	cfg     controllerConfig
	manager serviceManager
	mu      sync.Mutex
}

type revisionRecord struct {
	Revision     string `json:"revision"`
	ConfigSHA256 string `json:"configSha256"`
}

// NewDefaultSystemdController creates the production controller. Every path and
// the unit name are compile-time constants; no Inbound field can alter them.
func NewDefaultSystemdController() Controller {
	uid := uint32(0)
	if current := os.Geteuid(); current >= 0 {
		uid = uint32(current)
	}
	return newSystemdController(controllerConfig{
		unitName:          DefaultSystemdUnit,
		binaryPath:        DefaultGhostLinkdBinary,
		configPath:        DefaultGhostLinkdConfig,
		stateDBPath:       DefaultGhostLinkdStateDB,
		startWait:         8 * time.Second,
		pollInterval:      100 * time.Millisecond,
		preflightPorts:    true,
		controlSocket:     DefaultControlSocketPath,
		controlPeerUID:    uid,
		controlServerUID:  uid,
		userOperationPath: DefaultUserOperationPath,
	}, &systemctlManager{path: DefaultSystemctlBinary, unit: DefaultSystemdUnit})
}

func newSystemdController(cfg controllerConfig, manager serviceManager) *systemdController {
	if cfg.startWait <= 0 {
		cfg.startWait = 8 * time.Second
	}
	if cfg.pollInterval <= 0 {
		cfg.pollInterval = 100 * time.Millisecond
	}
	if cfg.controlClient == nil && cfg.controlSocket != "" {
		cfg.controlClient = newLocalControlClient(cfg.controlSocket, cfg.controlServerUID)
	}
	return &systemdController{cfg: cfg, manager: manager}
}

func (c *systemdController) Reconcile(ctx context.Context, inbound *model.Inbound) (Status, error) {
	return c.reconcileWithPolicies(ctx, inbound, nil, false)
}

func (c *systemdController) ReconcileGhostLinkWithPolicies(
	ctx context.Context,
	inbound *model.Inbound,
	policies map[string]RuntimePolicy,
) (Status, error) {
	return c.reconcileWithPolicies(ctx, inbound, policies, true)
}

func (c *systemdController) reconcileWithPolicies(
	ctx context.Context,
	inbound *model.Inbound,
	policies map[string]RuntimePolicy,
	allowZeroEnabled bool,
) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if inbound == nil {
		return c.stopLocked(ctx, "no GhostLink inbound")
	}
	if inbound.Protocol != model.GhostLink {
		return Status{Unit: c.cfg.unitName, Reason: "not_ghostlink"}, errors.New("not a GhostLink inbound")
	}
	state, err := BuildDesiredStateWithPolicies(
		inbound,
		c.cfg.stateDBPath,
		c.cfg.controlPeerUID,
		c.cfg.controlSocket,
		c.cfg.allowPrivateTargets,
		policies,
	)
	if err != nil {
		return Status{
			Unit:     c.cfg.unitName,
			Expected: inbound.Enable,
			Listen:   desiredListen(inbound),
			Reason:   "invalid_desired_state",
		}, err
	}
	if !state.ShouldRun {
		status, stopErr := c.stopLocked(ctx, state.Reason)
		if inbound.Enable && !allowZeroEnabled {
			return status, errors.Join(
				errors.New("GhostLink cannot start without at least one enabled user"),
				stopErr,
			)
		}
		return status, stopErr
	}
	return c.applyLocked(ctx, state)
}

func (c *systemdController) Stop(ctx context.Context) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopLocked(ctx, "stopped")
}

func (c *systemdController) Status(ctx context.Context, inbound *model.Inbound) (Status, error) {
	return c.GhostLinkStatusWithPolicies(ctx, inbound, nil)
}

func (c *systemdController) GhostLinkStatusWithPolicies(
	ctx context.Context,
	inbound *model.Inbound,
	policies map[string]RuntimePolicy,
) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	status := Status{Unit: c.cfg.unitName, Reason: "inactive"}
	desiredUserRevision := ""
	desiredTransport := ""
	if inbound != nil {
		if inbound.Protocol != model.GhostLink {
			status.Reason = "not_ghostlink"
			return status, errors.New("not a GhostLink inbound")
		}
		state, err := BuildDesiredStateWithPolicies(
			inbound,
			c.cfg.stateDBPath,
			c.cfg.controlPeerUID,
			c.cfg.controlSocket,
			c.cfg.allowPrivateTargets,
			policies,
		)
		if err != nil {
			status.Expected = inbound.Enable
			status.Listen = desiredListen(inbound)
			status.Reason = "invalid_desired_state"
			return status, err
		}
		status.Expected = state.ShouldRun
		status.Listen = state.Config.Listen
		status.DesiredRevision = state.Revision
		status.DesiredUserRevision = state.UserRevision
		desiredTransport = state.Config.Transport
		desiredUserRevision = state.UserRevision
		if !state.ShouldRun {
			status.Reason = state.Reason
		}
	}

	snapshot, err := c.manager.Snapshot(ctx)
	if err != nil {
		status.Reason = "systemd_unavailable"
		return status, err
	}
	if !snapshot.Loaded {
		status.Reason = "unit_not_installed"
		return status, fmt.Errorf("systemd unit %s is not installed", c.cfg.unitName)
	}
	status.Running = snapshot.Active
	status.MainPID = snapshot.PID
	status.ActiveRevision = c.readRevision()
	if status.Running {
		if err := c.verifyRuntime(ctx, &status, snapshot, status.Listen, desiredTransport, desiredUserRevision); err != nil {
			status.Ready = false
			return status, nil
		}
	}

	switch {
	case status.Expected && status.Running && status.RuntimeReady &&
		status.ActiveRevision == status.DesiredRevision:
		status.Reason = "running"
		status.Ready = true
	case status.Expected && status.Running:
		status.Reason = "revision_mismatch"
		status.Ready = false
	case status.Expected:
		status.Reason = "desired_enabled_but_inactive"
		status.Ready = false
	case status.Running:
		status.Reason = "desired_disabled_but_running"
		status.Ready = false
	default:
		status.Ready = false
		if status.Reason == "" {
			status.Reason = "inactive"
		}
	}
	return status, nil
}

func (c *systemdController) applyLocked(ctx context.Context, state DesiredState) (Status, error) {
	status := Status{
		Unit:                c.cfg.unitName,
		Expected:            true,
		Listen:              state.Config.Listen,
		DesiredRevision:     state.Revision,
		DesiredUserRevision: state.UserRevision,
		Reason:              "starting",
	}
	if err := c.validateFixedPaths(); err != nil {
		status.Reason = "invalid_controller_paths"
		return status, err
	}
	if err := ensurePrivateDir(filepath.Dir(c.cfg.stateDBPath)); err != nil {
		status.Reason = "state_dir_failed"
		return status, err
	}
	if state.UserCount == 0 || state.EnabledUsers == 0 {
		status.Reason = "waiting_for_enabled_users"
		return status, errors.New("GhostLink cannot start without at least one enabled user")
	}
	if err := c.validateBinary(); err != nil {
		status.Reason = "binary_unavailable"
		return status, err
	}

	snapshot, err := c.manager.Snapshot(ctx)
	if err != nil {
		status.Reason = "systemd_unavailable"
		return status, err
	}
	if !snapshot.Loaded {
		status.Reason = "unit_not_installed"
		return status, fmt.Errorf("systemd unit %s is not installed", c.cfg.unitName)
	}
	status.Running = snapshot.Active
	status.MainPID = snapshot.PID

	configBytes, err := state.ProtectedJSON()
	if err != nil {
		status.Reason = "config_encode_failed"
		return status, err
	}
	oldConfig, oldConfigExists, err := readOptionalFile(c.cfg.configPath)
	if err != nil {
		status.Reason = "config_read_failed"
		return status, err
	}
	oldRecord, oldRecordExists, err := c.readRevisionRecord()
	if err != nil {
		status.Reason = "revision_read_failed"
		return status, err
	}
	configRevision := digest(configBytes)
	configMatches := oldConfigExists && bytes.Equal(oldConfig, configBytes)
	recordMatches := oldRecordExists &&
		oldRecord.Revision == state.Revision &&
		oldRecord.ConfigSHA256 == configRevision

	if snapshot.Active && configMatches {
		if err := c.waitReady(ctx, &status, state.Config.Listen, state.Config.Transport, state.UserRevision); err != nil {
			status.Reason = "runtime_not_ready"
			return status, err
		}
		if !recordMatches {
			if err := c.writeRevision(state.Revision, configRevision); err != nil {
				status.Reason = "revision_write_failed"
				return status, err
			}
		}
		status.Running = true
		status.Ready = true
		status.MainPID = snapshot.PID
		status.ActiveRevision = state.Revision
		status.Reason = "running"
		return status, nil
	}

	if !snapshot.Active && c.cfg.preflightPorts {
		if err := ensurePortsAvailable(state.Config.Listen, state.Config.Transport); err != nil {
			status.Reason = "port_unavailable"
			return status, err
		}
	}

	if !configMatches {
		if err := c.writeProtected(c.cfg.configPath, configBytes); err != nil {
			status.Reason = "config_write_failed"
			return status, err
		}
	}

	restartNeeded := snapshot.Active
	if restartNeeded {
		err = c.manager.Restart(ctx)
	} else {
		err = c.manager.EnableNow(ctx)
	}
	if err == nil {
		err = c.waitReady(ctx, &status, state.Config.Listen, state.Config.Transport, state.UserRevision)
	}
	if err != nil {
		rollbackErr := c.rollback(ctx, oldConfig, oldConfigExists, oldRecord, oldRecordExists, snapshot.Active)
		status.Running = false
		status.MainPID = 0
		status.Reason = "start_failed"
		return status, errors.Join(err, rollbackErr)
	}
	if err := c.writeRevision(state.Revision, configRevision); err != nil {
		rollbackErr := c.rollback(ctx, oldConfig, oldConfigExists, oldRecord, oldRecordExists, snapshot.Active)
		status.Running = false
		status.MainPID = 0
		status.Reason = "revision_write_failed"
		return status, errors.Join(err, rollbackErr)
	}

	status.Running = true
	status.Ready = true
	status.ActiveRevision = state.Revision
	status.Reason = "running"
	return status, nil
}

func (c *systemdController) stopLocked(ctx context.Context, reason string) (Status, error) {
	status := Status{Unit: c.cfg.unitName, Expected: false, Reason: reason}
	configExists := fileExists(c.cfg.configPath) || fileExists(c.revisionPath())
	if !configExists {
		if !c.manager.Available() {
			status.Reason = reason
			return status, nil
		}
		snapshot, err := c.manager.Snapshot(ctx)
		if err != nil {
			status.Reason = "systemd_unavailable"
			return status, err
		}
		if !snapshot.Loaded {
			status.Reason = reason
			return status, nil
		}
		// The config can be missing after an external cleanup or crash while the
		// fixed unit still runs. The unit name is hard-coded, so stopping it
		// cannot touch another service.
		if err := c.manager.DisableNow(ctx); err != nil {
			status.Reason = "stop_failed"
			return status, err
		}
		c.removeConfig()
		status.Running = false
		status.Ready = false
		status.MainPID = 0
		status.Reason = reason
		return status, nil
	}

	snapshot, err := c.manager.Snapshot(ctx)
	if err != nil {
		status.Reason = "systemd_unavailable"
		return status, err
	}
	if !snapshot.Loaded {
		if configExists {
			status.Reason = "unit_not_installed"
			return status, fmt.Errorf("systemd unit %s is not installed", c.cfg.unitName)
		}
		c.removeConfig()
		status.Reason = reason
		return status, nil
	}
	status.Running = snapshot.Active
	status.MainPID = snapshot.PID
	if snapshot.Active || configExists {
		if err := c.manager.DisableNow(ctx); err != nil {
			status.Reason = "stop_failed"
			return status, err
		}
	}
	c.removeConfig()
	status.Running = false
	status.Ready = false
	status.MainPID = 0
	status.ActiveRevision = ""
	status.Reason = reason
	return status, nil
}

func (c *systemdController) rollback(
	ctx context.Context,
	oldConfig []byte,
	oldConfigExists bool,
	oldRecord revisionRecord,
	oldRecordExists bool,
	wasActive bool,
) error {
	var errs []error
	if oldConfigExists {
		if err := c.writeProtected(c.cfg.configPath, oldConfig); err != nil {
			errs = append(errs, fmt.Errorf("restore previous config: %w", err))
		} else if oldRecordExists {
			if err := c.writeRevisionRecord(oldRecord); err != nil {
				errs = append(errs, fmt.Errorf("restore previous revision: %w", err))
			}
		} else {
			_ = os.Remove(c.revisionPath())
		}
		if wasActive {
			if err := c.manager.Restart(ctx); err != nil {
				errs = append(errs, fmt.Errorf("restart previous config: %w", err))
			}
		} else if err := c.manager.DisableNow(ctx); err != nil && !isSystemdNotLoadedError(err) {
			errs = append(errs, fmt.Errorf("disable unsuccessful start: %w", err))
		}
	} else {
		if err := c.manager.DisableNow(ctx); err != nil && !isSystemdNotLoadedError(err) {
			errs = append(errs, fmt.Errorf("disable failed start: %w", err))
		}
		c.removeConfig()
	}
	return errors.Join(errs...)
}

func listenerMatches(actual, desired string) bool {
	if actual == desired {
		return true
	}
	actualHost, actualPort, err1 := net.SplitHostPort(actual)
	desiredHost, desiredPort, err2 := net.SplitHostPort(desired)
	if err1 != nil || err2 != nil || actualPort != desiredPort {
		return false
	}
	isWildcard := func(h string) bool {
		return h == "" || h == "0.0.0.0" || h == "::" || h == "[::]"
	}
	return isWildcard(actualHost) && isWildcard(desiredHost)
}

func (c *systemdController) verifyRuntime(
	ctx context.Context,
	status *Status,
	snapshot unitSnapshot,
	listen string,
	transport string,
	userRevision string,
) error {
	status.RuntimeReady = false
	if c.cfg.controlClient == nil {
		status.Reason = "local_control_not_configured"
		return errors.New("GhostLink local control client is not configured")
	}
	runtimeStatus, err := c.cfg.controlClient.Status(ctx)
	if err != nil {
		status.Reason = "local_control_unavailable"
		return err
	}
	status.RuntimePID = runtimeStatus.PeerPID
	status.InstanceID = runtimeStatus.InstanceID
	status.RuntimeRevision = runtimeStatus.UserRevision
	if runtimeStatus.PeerPID != snapshot.PID || runtimeStatus.PID != snapshot.PID {
		status.Reason = "runtime_identity_mismatch"
		return errors.New("GhostLink local control peer PID does not match systemd MainPID")
	}
	if runtimeStatus.PeerUID != c.cfg.controlServerUID {
		status.Reason = "runtime_identity_mismatch"
		return errors.New("GhostLink local control peer UID does not match the fixed service identity")
	}
	if !runtimeStatus.Serving {
		status.Reason = "runtime_not_serving"
		return errors.New("GhostLink runtime is not serving")
	}
	listenersMatch := false
	switch transport {
	case TransportH2:
		listenersMatch = listenerMatches(runtimeStatus.H2Listen, listen) && runtimeStatus.H3Listen == ""
	case TransportDualMASQUE:
		listenersMatch = listenerMatches(runtimeStatus.H2Listen, listen) && listenerMatches(runtimeStatus.H3Listen, listen)
	}
	if listen == "" || !listenersMatch {
		status.Reason = "runtime_listen_mismatch"
		return errors.New("GhostLink listener addresses do not match the desired transport")
	}
	if runtimeStatus.ActiveUsers == 0 {
		status.Reason = "runtime_has_no_active_users"
		return errors.New("GhostLink runtime has no enabled users")
	}
	if userRevision == "" || runtimeStatus.UserRevision != userRevision {
		status.Reason = "runtime_user_revision_mismatch"
		return errors.New("GhostLink runtime user revision does not match desired config")
	}
	status.RuntimeReady = true
	status.Reason = "running"
	return nil
}

func (c *systemdController) waitReady(
	ctx context.Context,
	status *Status,
	listen string,
	transport string,
	userRevision string,
) error {
	deadline := time.NewTimer(c.cfg.startWait)
	defer deadline.Stop()
	ticker := time.NewTicker(c.cfg.pollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		snapshot, err := c.manager.Snapshot(ctx)
		if err != nil {
			lastErr = err
		} else {
			status.Running = snapshot.Active
			status.MainPID = snapshot.PID
			if snapshot.Loaded && snapshot.Active && snapshot.PID > 0 {
				if err := c.verifyRuntime(ctx, status, snapshot, listen, transport, userRevision); err == nil {
					return nil
				} else {
					lastErr = err
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), lastErr)
		case <-deadline.C:
			if lastErr != nil {
				return fmt.Errorf("systemd service did not become ready: %w", lastErr)
			}
			return fmt.Errorf("systemd service did not become active and listen within %s", c.cfg.startWait)
		case <-ticker.C:
		}
	}
}

func (c *systemdController) validateFixedPaths() error {
	if c.cfg.unitName != DefaultSystemdUnit && !strings.HasPrefix(c.cfg.unitName, "ghostlinkd-3xui-smoke-") {
		return fmt.Errorf("refusing non-dedicated GhostLink unit %q", c.cfg.unitName)
	}
	for field, path := range map[string]string{
		"binary":  c.cfg.binaryPath,
		"config":  c.cfg.configPath,
		"stateDB": c.cfg.stateDBPath,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("GhostLink %s path must be an absolute clean path", field)
		}
	}
	if !c.cfg.container && filepath.Dir(c.cfg.configPath) == filepath.Dir(c.cfg.stateDBPath) {
		return errors.New("GhostLink config and state database must use separate dedicated directories")
	}
	if c.cfg.allowTestPaths {
		if c.cfg.controlSocket == "" {
			return errors.New("GhostLink local control socket path is required")
		}
	} else if c.cfg.controlSocket != DefaultControlSocketPath {
		clean := filepath.Clean(c.cfg.controlSocket)
		parent := filepath.Dir(clean)
		if clean != c.cfg.controlSocket || !filepath.IsAbs(clean) ||
			filepath.Base(clean) != "control.sock" ||
			!strings.HasPrefix(parent+"/", TestControlSocketRoot) {
			return errors.New("GhostLink local control socket path is not allowed")
		}
	}
	if c.cfg.allowTestPaths {
		if c.cfg.userOperationPath == "" {
			return errors.New("GhostLink user operation journal path is required")
		}
	} else if c.cfg.userOperationPath != DefaultUserOperationPath &&
		!(c.cfg.container && c.cfg.userOperationPath == filepath.Join(ContainerRoot, "pending-user-op.json")) {
		return errors.New("GhostLink user operation journal path is not allowed")
	}
	return nil
}

func (c *systemdController) validateBinary() error {
	if (c.cfg.container || (c.cfg.binaryPath == DefaultGhostLinkdBinary && os.Getenv("GHOSTLINK_LICENSE_HUB_URL") != "")) &&
		!DefaultInstaller().Ready() {
		return ErrInstallRequired
	}
	info, err := os.Stat(c.cfg.binaryPath)
	if err != nil {
		return fmt.Errorf("GhostLink binary is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("GhostLink binary path is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return errors.New("GhostLink binary is not executable")
	}
	return nil
}

func (c *systemdController) revisionPath() string {
	return filepath.Join(filepath.Dir(c.cfg.configPath), ghostLinkRevisionFileName)
}

func (c *systemdController) writeRevision(revision, configHash string) error {
	return c.writeRevisionRecord(revisionRecord{Revision: revision, ConfigSHA256: configHash})
}

func (c *systemdController) writeRevisionRecord(record revisionRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return c.writeProtected(c.revisionPath(), raw)
}

func (c *systemdController) readRevisionRecord() (revisionRecord, bool, error) {
	raw, exists, err := readOptionalFile(c.revisionPath())
	if err != nil || !exists {
		return revisionRecord{}, exists, err
	}
	var record revisionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return revisionRecord{}, false, nil
	}
	return record, true, nil
}

func (c *systemdController) readRevision() string {
	record, exists, err := c.readRevisionRecord()
	if err != nil || !exists {
		return ""
	}
	return record.Revision
}

func (c *systemdController) writeProtected(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		parent, err := os.Open(dir)
		if err != nil {
			return err
		}
		syncErr := parent.Sync()
		closeErr := parent.Close()
		return errors.Join(syncErr, closeErr)
	}
	return nil
}

func (c *systemdController) removeConfig() {
	_ = os.Remove(c.cfg.configPath)
	_ = os.Remove(c.revisionPath())
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func ensurePortsAvailable(listen, transport string) error {
	if transport == TransportH2 || transport == TransportDualMASQUE {
		tcp, err := net.Listen("tcp", listen)
		if err != nil {
			return fmt.Errorf("GhostLink TCP/H2 port %s is unavailable: %w", listen, err)
		}
		tcp.Close()
	}
	if transport == TransportDualMASQUE {
		udp, err := net.ListenPacket("udp", listen)
		if err != nil {
			return fmt.Errorf("GhostLink UDP/H3 port %s is unavailable: %w", listen, err)
		}
		udp.Close()
	}
	return nil
}

// portsBound is used only after a successful free-port preflight and an active
// MainPID, so both bind failures indicate that the managed process now owns the
// shared numeric TCP+UDP port.
func portsBound(listen string) bool {
	tcp, err := net.Listen("tcp", listen)
	if err == nil {
		tcp.Close()
		return false
	}
	udp, err := net.ListenPacket("udp", listen)
	if err == nil {
		udp.Close()
		return false
	}
	return true
}

func desiredListen(inbound *model.Inbound) string {
	if inbound == nil {
		return ""
	}
	host := strings.TrimSpace(inbound.Listen)
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.Itoa(inbound.Port))
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readOptionalFile(path string) ([]byte, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isSystemdNotLoadedError(err error) bool {
	return err == nil || strings.Contains(err.Error(), "not loaded")
}
