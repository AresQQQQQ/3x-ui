package ghostlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
)

type userOperationJournal struct {
	Version         int    `json:"version"`
	OperationID     string `json:"operation_id"`
	RequestID       string `json:"request_id"`
	Phase           string `json:"phase"`
	OldConfig       []byte `json:"old_config"`
	NewConfig       []byte `json:"new_config"`
	OldBootRevision string `json:"old_boot_revision"`
	NewBootRevision string `json:"new_boot_revision"`
	OldUserRevision string `json:"old_user_revision"`
	NewUserRevision string `json:"new_user_revision"`
	OldShouldRun    bool   `json:"old_should_run"`
	NewShouldRun    bool   `json:"new_should_run"`
	Hot             bool   `json:"hot"`
	PIDBefore       int    `json:"pid_before"`
}

func (c *systemdController) ApplyUserConfig(
	ctx context.Context,
	oldInbound *model.Inbound,
	newInbound *model.Inbound,
	expectedRuntimeRevision string,
	requestID string,
) (UserOperation, error) {
	return c.applyUserConfig(
		ctx,
		oldInbound,
		newInbound,
		nil,
		nil,
		expectedRuntimeRevision,
		requestID,
	)
}

func (c *systemdController) ApplyUserPolicyConfig(
	ctx context.Context,
	oldInbound *model.Inbound,
	newInbound *model.Inbound,
	oldPolicies map[string]RuntimePolicy,
	newPolicies map[string]RuntimePolicy,
	expectedRuntimeRevision string,
	requestID string,
) (UserOperation, error) {
	return c.applyUserConfig(
		ctx,
		oldInbound,
		newInbound,
		oldPolicies,
		newPolicies,
		expectedRuntimeRevision,
		requestID,
	)
}

func (c *systemdController) applyUserConfig(
	ctx context.Context,
	oldInbound *model.Inbound,
	newInbound *model.Inbound,
	oldPolicies map[string]RuntimePolicy,
	newPolicies map[string]RuntimePolicy,
	expectedRuntimeRevision string,
	requestID string,
) (UserOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := validateUserOperationID(requestID); err != nil {
		return UserOperation{}, err
	}
	if err := c.validateFixedPaths(); err != nil {
		return UserOperation{}, err
	}
	if pending, exists, err := c.pendingUserConfigLocked(); err != nil {
		return UserOperation{}, err
	} else if exists {
		return UserOperation{}, fmt.Errorf("GhostLink user operation %s is pending recovery", pending.OperationID)
	}
	oldState, err := BuildDesiredStateWithPolicies(
		oldInbound,
		c.cfg.stateDBPath,
		c.cfg.controlPeerUID,
		c.cfg.controlSocket,
		c.cfg.allowPrivateTargets,
		oldPolicies,
	)
	if err != nil {
		return UserOperation{}, fmt.Errorf("build previous GhostLink user state: %w", err)
	}
	newState, err := BuildDesiredStateWithPolicies(
		newInbound,
		c.cfg.stateDBPath,
		c.cfg.controlPeerUID,
		c.cfg.controlSocket,
		c.cfg.allowPrivateTargets,
		newPolicies,
	)
	if err != nil {
		return UserOperation{}, fmt.Errorf("build next GhostLink user state: %w", err)
	}
	oldShouldRun := oldState.ShouldRun && oldState.EnabledUsers > 0
	newShouldRun := newState.ShouldRun && newState.EnabledUsers > 0
	if newShouldRun && newState.EnabledUsers == 0 {
		return UserOperation{}, errors.New("GhostLink cannot apply a running state with zero enabled users")
	}
	snapshot, err := c.manager.Snapshot(ctx)
	if err != nil {
		return UserOperation{}, err
	}
	if oldShouldRun {
		if !snapshot.Active {
			return UserOperation{}, errors.New("GhostLink is not active; reconcile lifecycle before changing users")
		}
		current := Status{Listen: oldState.Config.Listen, DesiredUserRevision: oldState.UserRevision}
		if err := c.verifyRuntime(ctx, &current, snapshot, oldState.Config.Listen, oldState.Config.Transport, oldState.UserRevision); err != nil {
			return UserOperation{}, fmt.Errorf("current GhostLink runtime is not ready for user mutation: %w", err)
		}
		if expectedRuntimeRevision == "" || current.RuntimeRevision != expectedRuntimeRevision {
			return UserOperation{}, errors.New("GhostLink active user revision changed before update")
		}
	}

	oldConfig, err := oldState.ProtectedJSON()
	if err != nil {
		return UserOperation{}, err
	}
	newConfig, err := newState.ProtectedJSON()
	if err != nil {
		return UserOperation{}, err
	}
	hot := oldShouldRun && newShouldRun &&
		oldState.Config.Listen == newState.Config.Listen &&
		snapshot.Active
	journal := userOperationJournal{
		Version:         1,
		OperationID:     requestID,
		RequestID:       requestID,
		Phase:           "prepared",
		OldConfig:       oldConfig,
		NewConfig:       newConfig,
		OldBootRevision: oldState.Revision,
		NewBootRevision: newState.Revision,
		OldUserRevision: oldState.UserRevision,
		NewUserRevision: newState.UserRevision,
		OldShouldRun:    oldShouldRun,
		NewShouldRun:    newShouldRun,
		Hot:             hot,
		PIDBefore:       snapshot.PID,
	}
	if err := c.writeUserJournal(journal); err != nil {
		return UserOperation{}, err
	}

	operation := UserOperation{
		OperationID:     journal.OperationID,
		RequestID:       requestID,
		OldUserRevision: oldState.UserRevision,
		NewUserRevision: newState.UserRevision,
		Hot:             hot,
		PIDBefore:       snapshot.PID,
		PIDAfter:        snapshot.PID,
	}
	var applyErr error
	switch {
	case !newShouldRun:
		reason := newState.Reason
		if reason == "" {
			reason = "waiting_for_enabled_users"
		}
		operation.Status, applyErr = c.stopLocked(ctx, reason)
		operation.PIDAfter = 0
	case hot:
		operation.Status, applyErr = c.applyUsersHotLocked(
			ctx,
			newState,
			oldState.UserRevision,
			requestID,
		)
	default:
		operation.Status, applyErr = c.applyLocked(ctx, newState)
		operation.PIDAfter = operation.Status.MainPID
	}
	if applyErr != nil {
		rollbackErr := c.rollbackUserJournalLocked(ctx, &journal)
		if rollbackErr == nil {
			rollbackErr = c.removeUserJournal()
		}
		return operation, errors.Join(applyErr, rollbackErr)
	}
	journal.Phase = "applied"
	if err := c.writeUserJournal(journal); err != nil {
		rollbackErr := c.rollbackUserJournalLocked(ctx, &journal)
		if rollbackErr == nil {
			rollbackErr = c.removeUserJournal()
		}
		return operation, errors.Join(err, rollbackErr)
	}
	return operation, nil
}

func (c *systemdController) ConfirmUserConfig(ctx context.Context, operationID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	journal, exists, err := c.readUserJournal()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if operationID != "" && journal.OperationID != operationID {
		return fmt.Errorf("GhostLink pending user operation %s does not match confirmation %s", journal.OperationID, operationID)
	}
	if err := c.ensureUserJournalTargetLocked(ctx, journal, true); err != nil {
		return err
	}
	return c.removeUserJournal()
}

func (c *systemdController) RollbackUserConfig(ctx context.Context, operationID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	journal, exists, err := c.readUserJournal()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if operationID != "" && journal.OperationID != operationID {
		return fmt.Errorf("GhostLink pending user operation %s does not match rollback %s", journal.OperationID, operationID)
	}
	if err := c.rollbackUserJournalLocked(ctx, &journal); err != nil {
		return err
	}
	return c.removeUserJournal()
}

func (c *systemdController) ResolvePendingUserConfig(
	ctx context.Context,
	desiredUserRevision string,
) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	journal, exists, err := c.readUserJournal()
	if err != nil {
		return "", err
	}
	if !exists {
		return "none", nil
	}
	switch desiredUserRevision {
	case journal.NewUserRevision:
		if err := c.ensureUserJournalTargetLocked(ctx, journal, false); err != nil {
			return "", err
		}
		if err := c.removeUserJournal(); err != nil {
			return "", err
		}
		return "rolled_forward", nil
	case journal.OldUserRevision:
		if err := c.rollbackUserJournalLocked(ctx, &journal); err != nil {
			return "", err
		}
		if err := c.removeUserJournal(); err != nil {
			return "", err
		}
		return "rolled_back", nil
	default:
		return "", fmt.Errorf(
			"GhostLink pending operation %s matches neither current DB user revision %s nor either journal revision",
			journal.OperationID,
			desiredUserRevision,
		)
	}
}

func (c *systemdController) PendingUserConfig(context.Context) (PendingUserOperation, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingUserConfigLocked()
}

func (c *systemdController) pendingUserConfigLocked() (PendingUserOperation, bool, error) {
	journal, exists, err := c.readUserJournal()
	if err != nil || !exists {
		return PendingUserOperation{}, exists, err
	}
	return PendingUserOperation{
		OperationID:     journal.OperationID,
		RequestID:       journal.RequestID,
		Phase:           journal.Phase,
		OldUserRevision: journal.OldUserRevision,
		NewUserRevision: journal.NewUserRevision,
		OldShouldRun:    journal.OldShouldRun,
		NewShouldRun:    journal.NewShouldRun,
		Hot:             journal.Hot,
		PIDBefore:       journal.PIDBefore,
	}, true, nil
}

func (c *systemdController) applyUsersHotLocked(
	ctx context.Context,
	state DesiredState,
	expectedRevision string,
	requestID string,
) (Status, error) {
	if c.cfg.controlClient == nil {
		return Status{}, errors.New("GhostLink local control client is not configured")
	}
	snapshot, err := c.manager.Snapshot(ctx)
	if err != nil {
		return Status{}, err
	}
	if !snapshot.Active || snapshot.PID <= 0 {
		return Status{}, errors.New("GhostLink systemd unit is not active for hot user update")
	}
	current := Status{Listen: state.Config.Listen}
	if err := c.verifyRuntime(ctx, &current, snapshot, state.Config.Listen, state.Config.Transport, expectedRevision); err != nil {
		return Status{}, err
	}
	response, applyErr := c.cfg.controlClient.ApplyUsers(ctx, requestID, expectedRevision, state.Config.Users)
	if applyErr != nil {
		readBack, readErr := c.cfg.controlClient.Status(ctx)
		if readErr != nil || readBack.UserRevision != state.UserRevision {
			return Status{}, errors.Join(applyErr, readErr)
		}
	} else if response.UserRevision != state.UserRevision {
		return Status{}, errors.New("GhostLink runtime acknowledged an unexpected user revision")
	}
	if err := c.writeBootConfig(state); err != nil {
		return Status{}, err
	}
	if err := c.verifyRuntime(ctx, &current, snapshot, state.Config.Listen, state.Config.Transport, state.UserRevision); err != nil {
		return Status{}, err
	}
	current.Expected = true
	current.Running = true
	current.Ready = true
	current.RuntimeReady = true
	current.MainPID = snapshot.PID
	current.RuntimePID = snapshot.PID
	current.DesiredRevision = state.Revision
	current.ActiveRevision = state.Revision
	current.DesiredUserRevision = state.UserRevision
	current.RuntimeRevision = state.UserRevision
	current.Reason = "running"
	return current, nil
}

func (c *systemdController) writeBootConfig(state DesiredState) error {
	config, err := state.ProtectedJSON()
	if err != nil {
		return err
	}
	if err := c.writeProtected(c.cfg.configPath, config); err != nil {
		return err
	}
	return c.writeRevision(state.Revision, digest(config))
}

func (c *systemdController) ensureUserJournalTargetLocked(
	ctx context.Context,
	journal userOperationJournal,
	requireSamePID bool,
) error {
	if !journal.NewShouldRun {
		if _, err := c.stopLocked(ctx, "waiting_for_enabled_users"); err != nil {
			return err
		}
		return nil
	}
	state, err := desiredStateFromConfig(
		journal.NewConfig,
		journal.NewBootRevision,
		journal.NewUserRevision,
		true,
	)
	if err != nil {
		return err
	}
	snapshot, err := c.manager.Snapshot(ctx)
	if err != nil {
		return err
	}
	if snapshot.Active {
		if requireSamePID && journal.Hot && snapshot.PID != journal.PIDBefore {
			return fmt.Errorf("GhostLink hot user operation changed MainPID from %d to %d", journal.PIDBefore, snapshot.PID)
		}
		if err := c.verifyRuntime(ctx, &Status{}, snapshot, state.Config.Listen, state.Config.Transport, state.UserRevision); err == nil {
			currentConfig, exists, readErr := readOptionalFile(c.cfg.configPath)
			if readErr == nil && exists && bytes.Equal(currentConfig, journal.NewConfig) {
				record, recordExists, recordErr := c.readRevisionRecord()
				if recordErr == nil && recordExists &&
					record.Revision == journal.NewBootRevision &&
					record.ConfigSHA256 == digest(currentConfig) {
					return nil
				}
			}
		}
	}
	if _, err := c.applyLocked(ctx, state); err != nil {
		return err
	}
	return nil
}

func (c *systemdController) rollbackUserJournalLocked(
	ctx context.Context,
	journal *userOperationJournal,
) error {
	if !journal.OldShouldRun {
		if _, err := c.stopLocked(ctx, "rolled_back"); err != nil {
			return err
		}
		return nil
	}
	oldState, err := desiredStateFromConfig(
		journal.OldConfig,
		journal.OldBootRevision,
		journal.OldUserRevision,
		true,
	)
	if err != nil {
		return err
	}
	snapshot, err := c.manager.Snapshot(ctx)
	if err != nil {
		return err
	}
	if snapshot.Active {
		if _, err := c.applyUsersHotLocked(ctx, oldState, journal.NewUserRevision, "rollback-"+journal.OperationID); err == nil {
			return nil
		}
	}
	if _, err := c.applyLocked(ctx, oldState); err != nil {
		return err
	}
	return nil
}

func desiredStateFromConfig(
	config []byte,
	bootRevision string,
	userRevision string,
	shouldRun bool,
) (DesiredState, error) {
	var parsed ManagementConfig
	if err := json.Unmarshal(config, &parsed); err != nil {
		return DesiredState{}, fmt.Errorf("decode GhostLink journal config: %w", err)
	}
	enabled := 0
	for _, user := range parsed.Users {
		if user.Enabled {
			enabled++
		}
	}
	return DesiredState{
		Revision:     bootRevision,
		UserRevision: userRevision,
		ShouldRun:    shouldRun,
		Reason:       "ready",
		UserCount:    len(parsed.Users),
		EnabledUsers: enabled,
		Config:       parsed,
	}, nil
}

func (c *systemdController) userJournalPath() string {
	if c.cfg.userOperationPath != "" {
		return c.cfg.userOperationPath
	}
	return DefaultUserOperationPath
}

func (c *systemdController) writeUserJournal(journal userOperationJournal) error {
	raw, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return c.writeProtected(c.userJournalPath(), raw)
}

func (c *systemdController) readUserJournal() (userOperationJournal, bool, error) {
	raw, exists, err := readOptionalFile(c.userJournalPath())
	if err != nil || !exists {
		return userOperationJournal{}, exists, err
	}
	var journal userOperationJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return userOperationJournal{}, false, fmt.Errorf("decode GhostLink user operation journal: %w", err)
	}
	if journal.Version != 1 || journal.OperationID == "" ||
		journal.OldUserRevision == "" || journal.NewUserRevision == "" {
		return userOperationJournal{}, false, errors.New("GhostLink user operation journal is invalid")
	}
	return journal, true, nil
}

func (c *systemdController) removeUserJournal() error {
	err := os.Remove(c.userJournalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func NewUserOperationID() (string, error) {
	return uuid.NewString(), nil
}

func validateUserOperationID(value string) error {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return errors.New("GhostLink user operation request ID must be 1-128 bytes without surrounding whitespace")
	}
	for _, b := range []byte(value) {
		if b < 0x21 || b > 0x7e {
			return errors.New("GhostLink user operation request ID must contain printable ASCII only")
		}
	}
	return nil
}
