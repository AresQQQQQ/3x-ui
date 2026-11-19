package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/util/common"

	"gorm.io/gorm"
)

var errGhostLinkConfigurationLocked = errors.New(
	"GhostLink is enabled; disable it before changing this bulk or inbound-level configuration; single-client changes use the transactional hot-update path",
)

var ghostLinkUserOperationMu sync.Mutex

type ghostLinkLifecycleRuntime interface {
	ReconcileGhostLink(context.Context, *model.Inbound) (ghostlink.Status, error)
	StopGhostLink(context.Context) (ghostlink.Status, error)
	GhostLinkStatus(context.Context, *model.Inbound) (ghostlink.Status, error)
}

type ghostLinkUserOperationRuntime interface {
	ApplyGhostLinkUserConfig(context.Context, *model.Inbound, *model.Inbound, string, string) (ghostlink.UserOperation, error)
	ApplyGhostLinkUserPolicyConfig(
		context.Context,
		*model.Inbound,
		*model.Inbound,
		map[string]ghostlink.RuntimePolicy,
		map[string]ghostlink.RuntimePolicy,
		string,
		string,
	) (ghostlink.UserOperation, error)
	ConfirmGhostLinkUserConfig(context.Context, string) error
	RollbackGhostLinkUserConfig(context.Context, string) error
	ResolvePendingGhostLinkUserConfig(context.Context, string) (string, error)
	PendingGhostLinkUserConfig(context.Context) (ghostlink.PendingUserOperation, bool, error)
}

func validateGhostLinkInbound(inbound *model.Inbound) error {
	if inbound.Protocol != model.GhostLink {
		return nil
	}
	if inbound.NodeID != nil {
		return common.NewError("GhostLink is local-only in the first integration version")
	}
	if err := ghostlink.RequireInstallation(); err != nil {
		return err
	}
	if err := ghostlink.ValidateInbound(inbound); err != nil {
		return err
	}
	return nil
}

func ensureNoEnabledGhostLinkInbounds(db *gorm.DB, inboundIDs []int) error {
	if len(inboundIDs) == 0 {
		return nil
	}
	var count int64
	if err := db.Model(&model.Inbound{}).
		Where("id IN ? AND protocol = ? AND enable = ? AND node_id IS NULL", inboundIDs, model.GhostLink, true).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return errGhostLinkConfigurationLocked
	}
	return nil
}

func ensureNoEnabledGhostLinkForClientEmails(db *gorm.DB, emails []string) error {
	if len(emails) == 0 {
		return nil
	}
	var count int64
	if err := db.Model(&model.Inbound{}).
		Joins("JOIN client_inbounds ci ON ci.inbound_id = inbounds.id").
		Joins("JOIN clients c ON c.id = ci.client_id").
		Where("c.email IN ? AND inbounds.protocol = ? AND inbounds.enable = ? AND inbounds.node_id IS NULL",
			emails, model.GhostLink, true).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return errGhostLinkConfigurationLocked
	}
	return nil
}

func (s *InboundService) IsGhostLinkClientEmail(email string) (bool, error) {
	if strings.TrimSpace(email) == "" {
		return false, nil
	}
	var count int64
	err := database.GetDB().Table("clients").
		Joins("JOIN client_inbounds ON client_inbounds.client_id = clients.id").
		Joins("JOIN inbounds ON inbounds.id = client_inbounds.inbound_id").
		Where("clients.email = ? AND inbounds.protocol = ?", email, model.GhostLink).
		Count(&count).Error
	return count > 0, err
}

func hasEnabledGhostLinkInbounds() (bool, error) {
	var count int64
	err := database.GetDB().Model(&model.Inbound{}).
		Where("protocol = ? AND enable = ? AND node_id IS NULL", model.GhostLink, true).
		Count(&count).Error
	return count > 0, err
}

func (s *InboundService) ghostLinkLifecycle(inbound *model.Inbound) (ghostLinkLifecycleRuntime, error) {
	rt, err := s.runtimeFor(inbound)
	if err != nil {
		return nil, err
	}
	lifecycle, ok := rt.(ghostLinkLifecycleRuntime)
	if !ok {
		return nil, errors.New("local runtime does not provide a GhostLink lifecycle controller")
	}
	return lifecycle, nil
}

func (s *InboundService) ghostLinkUserOperations(inbound *model.Inbound) (ghostLinkUserOperationRuntime, error) {
	rt, err := s.runtimeFor(inbound)
	if err != nil {
		return nil, err
	}
	operations, ok := rt.(ghostLinkUserOperationRuntime)
	if !ok {
		return nil, errors.New("local runtime does not provide transactional GhostLink user operations")
	}
	return operations, nil
}

func (s *InboundService) localGhostLinkInbound() (*model.Inbound, bool, error) {
	var inbounds []*model.Inbound
	if err := database.GetDB().
		Where("protocol = ? AND node_id IS NULL", model.GhostLink).
		Order("id ASC").
		Find(&inbounds).Error; err != nil {
		return nil, false, err
	}
	switch len(inbounds) {
	case 0:
		return nil, false, nil
	case 1:
		return inbounds[0], true, nil
	default:
		return nil, false, errors.New("multiple GhostLink inbounds found; refusing to reconcile an ambiguous single-process desired state")
	}
}

// ReconcileGhostLink converges the fixed systemd-managed process to the
// persisted local inbound, or stops it when there is no GhostLink inbound.
func (s *InboundService) ReconcileGhostLink(ctx context.Context) (ghostlink.Status, error) {
	ghostLinkUserOperationMu.Lock()
	defer ghostLinkUserOperationMu.Unlock()
	_, cycleErr := s.runGhostLinkUsagePolicyCycleLocked(ctx)
	status, statusErr := s.ghostLinkLifecycleStatusLocked(ctx)
	return status, errors.Join(cycleErr, statusErr)
}

func (s *InboundService) reconcileGhostLinkLocked(ctx context.Context) (ghostlink.Status, error) {
	inbound, found, err := s.localGhostLinkInbound()
	if err != nil {
		return ghostlink.Status{}, err
	}
	probe := &model.Inbound{Protocol: model.GhostLink}
	lifecycle, err := s.ghostLinkLifecycle(probe)
	if err != nil {
		return ghostlink.Status{}, err
	}
	if !found {
		if operations, opErr := s.ghostLinkUserOperations(probe); opErr == nil {
			if pending, exists, pendingErr := operations.PendingGhostLinkUserConfig(ctx); pendingErr != nil {
				return ghostlink.Status{}, pendingErr
			} else if exists {
				if _, resolveErr := operations.ResolvePendingGhostLinkUserConfig(ctx, pending.NewUserRevision); resolveErr != nil {
					return ghostlink.Status{}, resolveErr
				}
			}
		}
		return lifecycle.StopGhostLink(ctx)
	}
	if operations, opErr := s.ghostLinkUserOperations(inbound); opErr == nil {
		policies, policyErr := currentGhostLinkRuntimePolicies(inbound)
		if policyErr != nil {
			return ghostlink.Status{}, policyErr
		}
		desiredRevision, revErr := ghostlink.UserRevisionForInboundWithPolicies(inbound, policies)
		if revErr != nil {
			return ghostlink.Status{}, revErr
		}
		if pending, exists, pendingErr := operations.PendingGhostLinkUserConfig(ctx); pendingErr != nil {
			return ghostlink.Status{}, pendingErr
		} else if exists {
			if _, resolveErr := operations.ResolvePendingGhostLinkUserConfig(ctx, desiredRevision); resolveErr != nil {
				return ghostlink.Status{}, fmt.Errorf("recover GhostLink user operation %s: %w", pending.OperationID, resolveErr)
			}
		}
	}
	if policyRuntime, ok := lifecycle.(ghostLinkPolicyRuntime); ok {
		policies, policyErr := currentGhostLinkRuntimePolicies(inbound)
		if policyErr != nil {
			return ghostlink.Status{}, policyErr
		}
		return policyRuntime.ReconcileGhostLinkWithPolicies(ctx, inbound, policies)
	}
	return ghostlink.Status{}, errors.New("local runtime does not provide GhostLink runtime policy updates")
}

// GhostLinkLifecycleStatus separates persisted desired state from actual systemd
// state and the active config revision. The returned payload contains no secret.
func (s *InboundService) GhostLinkLifecycleStatus(ctx context.Context) (ghostlink.Status, error) {
	ghostLinkUserOperationMu.Lock()
	defer ghostLinkUserOperationMu.Unlock()
	return s.ghostLinkLifecycleStatusLocked(ctx)
}

func (s *InboundService) ghostLinkLifecycleStatusLocked(ctx context.Context) (ghostlink.Status, error) {
	inbound, found, err := s.localGhostLinkInbound()
	if err != nil {
		return ghostlink.Status{}, err
	}
	probe := &model.Inbound{Protocol: model.GhostLink}
	lifecycle, err := s.ghostLinkLifecycle(probe)
	if err != nil {
		return ghostlink.Status{}, err
	}
	var status ghostlink.Status
	var statusErr error
	if found {
		policies, policyErr := currentGhostLinkRuntimePolicies(inbound)
		if policyErr != nil {
			return ghostlink.Status{}, policyErr
		}
		if policyRuntime, ok := lifecycle.(ghostLinkPolicyRuntime); ok {
			status, statusErr = policyRuntime.GhostLinkStatusWithPolicies(ctx, inbound, policies)
		} else {
			status, statusErr = lifecycle.GhostLinkStatus(ctx, inbound)
		}
	} else {
		status, statusErr = lifecycle.GhostLinkStatus(ctx, nil)
	}
	if operations, opErr := s.ghostLinkUserOperations(probe); opErr == nil {
		if pending, exists, pendingErr := operations.PendingGhostLinkUserConfig(ctx); pendingErr != nil {
			return status, errors.Join(statusErr, pendingErr)
		} else if exists {
			status.PendingOperationID = pending.OperationID
			status.PendingPhase = pending.Phase
		}
	}
	if found {
		if usageErr := decorateGhostLinkUsageStatus(&status, inbound.Id); usageErr != nil {
			return status, errors.Join(statusErr, usageErr)
		}
	} else {
		status.UsageState = "uninitialized"
	}
	return status, statusErr
}

func decorateGhostLinkUsageStatus(status *ghostlink.Status, inboundID int) error {
	var rows []model.GhostLinkUsageWatermark
	if err := database.GetDB().Where("inbound_id = ?", inboundID).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		status.UsageState = "uninitialized"
		return nil
	}
	status.UsageState = "ok"
	var latest int64
	policyState := ghostLinkPolicyOK
	policyError := ""
	for _, row := range rows {
		if row.LastSnapshotAt > latest {
			latest = row.LastSnapshotAt
		}
		if row.State == "pending" {
			status.UsageState = "pending"
			if status.UsageErrorCode == "" || row.ErrorCode < status.UsageErrorCode {
				status.UsageErrorCode = row.ErrorCode
			}
		} else if row.State == "baseline" && status.UsageState == "ok" {
			status.UsageState = "baseline"
		}
		if row.PolicyState == ghostLinkPolicyPendingUsage {
			policyState = ghostLinkPolicyPendingUsage
			if policyError == "" || row.PolicyErrorCode < policyError {
				policyError = row.PolicyErrorCode
			}
		} else if row.PolicyState != "" && row.PolicyState != ghostLinkPolicyOK &&
			policyState == ghostLinkPolicyOK {
			policyState = row.PolicyState
		}
	}
	status.UsageLastSnapshotAt = latest
	if status.UsageState != "pending" {
		status.UsageErrorCode = ""
	}
	status.PolicyState = policyState
	status.PolicyErrorCode = policyError
	return nil
}

func (s *InboundService) reconcileGhostLinkInbound(ctx context.Context, inbound *model.Inbound) (ghostlink.Status, error) {
	if inbound == nil || inbound.Protocol != model.GhostLink {
		return ghostlink.Status{}, fmt.Errorf("not a GhostLink inbound")
	}
	ghostLinkUserOperationMu.Lock()
	defer ghostLinkUserOperationMu.Unlock()
	return s.reconcileGhostLinkInboundLocked(ctx, inbound)
}

func (s *InboundService) reconcileGhostLinkInboundLocked(ctx context.Context, inbound *model.Inbound) (ghostlink.Status, error) {
	_, snapshot, usageErr := s.collectGhostLinkUsageSnapshotLocked(ctx, inbound, inbound)
	if usageErr != nil {
		var pending *GhostLinkUsagePendingError
		if !errors.As(usageErr, &pending) {
			return ghostlink.Status{}, usageErr
		}
	}
	policyErr := s.reconcileGhostLinkPolicySnapshotLocked(ctx, inbound, snapshot, nil)
	if policyErr != nil {
		return ghostlink.Status{}, errors.Join(usageErr, policyErr)
	}
	lifecycle, err := s.ghostLinkLifecycle(inbound)
	if err != nil {
		return ghostlink.Status{}, err
	}
	if policyRuntime, ok := lifecycle.(ghostLinkPolicyRuntime); ok {
		policies, policyLookupErr := currentGhostLinkRuntimePolicies(inbound)
		if policyLookupErr != nil {
			return ghostlink.Status{}, policyLookupErr
		}
		status, statusErr := policyRuntime.GhostLinkStatusWithPolicies(ctx, inbound, policies)
		return status, errors.Join(usageErr, policyErr, statusErr)
	}
	return ghostlink.Status{}, errors.Join(usageErr, policyErr, errors.New("local runtime does not provide GhostLink runtime policy updates"))
}

func (s *InboundService) stopGhostLink() (ghostlink.Status, error) {
	lifecycle, err := s.ghostLinkLifecycle(&model.Inbound{Protocol: model.GhostLink})
	if err != nil {
		return ghostlink.Status{}, err
	}
	return lifecycle.StopGhostLink(context.Background())
}

func (s *ClientService) applyGhostLinkSingleClientChange(
	inboundSvc *InboundService,
	oldInbound *model.Inbound,
	newInbound *model.Inbound,
	commit func(*gorm.DB) error,
) (bool, error) {
	if oldInbound == nil || newInbound == nil ||
		oldInbound.Protocol != model.GhostLink || newInbound.Protocol != model.GhostLink {
		return false, errors.New("GhostLink single-client transaction requires GhostLink inbounds")
	}
	ghostLinkUserOperationMu.Lock()
	defer ghostLinkUserOperationMu.Unlock()

	operations, err := inboundSvc.ghostLinkUserOperations(oldInbound)
	if err != nil {
		return false, err
	}
	lifecycle, err := inboundSvc.ghostLinkLifecycle(oldInbound)
	if err != nil {
		return false, err
	}
	usageRuntime, ok := lifecycle.(ghostLinkUsageRuntime)
	if !ok {
		return false, errors.New("GhostLink lifecycle controller does not support usage snapshots")
	}
	preCtx, preCancel := context.WithTimeout(context.Background(), 15*time.Second)
	preSnapshot, preErr := usageRuntime.GhostLinkUsageSnapshot(preCtx, oldInbound)
	preCancel()
	if preErr != nil {
		return false, fmt.Errorf("collect GhostLink usage before client change: %w", preErr)
	}
	if _, usageErr := inboundSvc.recordGhostLinkUsageSnapshot(context.Background(), oldInbound, preSnapshot); usageErr != nil {
		return false, fmt.Errorf("record GhostLink usage before client change: %w", usageErr)
	}
	oldPlan, err := inboundSvc.loadGhostLinkPolicyPlan(context.Background(), oldInbound, preSnapshot, nil)
	if err != nil {
		return false, fmt.Errorf("calculate current GhostLink policy: %w", err)
	}
	if oldPlan.pending {
		return false, errors.New("GhostLink usage is pending; client change is blocked until reconciliation completes")
	}
	currentRevision, err := ghostlink.UserRevisionForInboundWithPolicies(oldInbound, oldPlan.oldPolicies)
	if err != nil {
		return false, err
	}
	if pending, exists, pendingErr := operations.PendingGhostLinkUserConfig(context.Background()); pendingErr != nil {
		return false, pendingErr
	} else if exists {
		if _, resolveErr := operations.ResolvePendingGhostLinkUserConfig(context.Background(), currentRevision); resolveErr != nil {
			return false, fmt.Errorf("pending GhostLink user operation %s blocks update: %w", pending.OperationID, resolveErr)
		}
	}
	policyRuntime, ok := lifecycle.(ghostLinkPolicyRuntime)
	if !ok {
		return false, errors.New("GhostLink lifecycle controller does not support runtime policy updates")
	}
	oldStatus, oldStatusErr := policyRuntime.GhostLinkStatusWithPolicies(context.Background(), oldInbound, oldPlan.oldPolicies)
	oldEffective := 0
	for _, policy := range oldPlan.oldPolicies {
		if policy.Enabled {
			oldEffective++
		}
	}
	if oldEffective > 0 && (oldStatusErr != nil || !oldStatus.Ready) {
		if _, reconcileErr := policyRuntime.ReconcileGhostLinkWithPolicies(context.Background(), oldInbound, oldPlan.oldPolicies); reconcileErr != nil {
			if oldStatusErr != nil {
				return false, errors.Join(oldStatusErr, reconcileErr)
			}
			return false, reconcileErr
		}
		oldStatus, oldStatusErr = policyRuntime.GhostLinkStatusWithPolicies(context.Background(), oldInbound, oldPlan.oldPolicies)
	}
	if oldStatusErr != nil {
		return false, oldStatusErr
	}
	targetSnapshot := preSnapshot
	if identities, identityErr := ghostlink.ClientIdentityMap(newInbound); identityErr == nil {
		present := make(map[string]struct{}, len(targetSnapshot.Users))
		for _, user := range targetSnapshot.Users {
			present[user.ClientID] = struct{}{}
		}
		for clientID := range identities {
			if _, ok := present[clientID]; !ok {
				targetSnapshot.Users = append(targetSnapshot.Users, ghostlink.UsageUser{ClientID: clientID})
			}
		}
	}
	targetPlan, err := inboundSvc.loadGhostLinkPolicyPlan(context.Background(), newInbound, targetSnapshot, nil)
	if err != nil {
		return false, fmt.Errorf("calculate next GhostLink policy: %w", err)
	}
	if targetPlan.pending {
		return false, errors.New("GhostLink target policy depends on pending usage; client change is blocked")
	}
	requestID, err := ghostlink.NewUserOperationID()
	if err != nil {
		return false, err
	}
	operation, err := operations.ApplyGhostLinkUserPolicyConfig(
		context.Background(),
		oldInbound,
		newInbound,
		oldPlan.oldPolicies,
		targetPlan.policies,
		oldStatus.RuntimeRevision,
		requestID,
	)
	if err != nil {
		return false, fmt.Errorf("apply GhostLink user operation %s: %w", requestID, err)
	}
	postCtx, postCancel := context.WithTimeout(context.Background(), 15*time.Second)
	postSnapshot, postErr := usageRuntime.GhostLinkUsageSnapshot(postCtx, newInbound)
	postCancel()
	if postErr != nil {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		rollbackErr := operations.RollbackGhostLinkUserConfig(rollbackCtx, operation.OperationID)
		return false, errors.Join(
			fmt.Errorf("collect GhostLink usage after client change: %w", postErr),
			rollbackErr,
		)
	}
	if err := runSerializedTx(func(tx *gorm.DB) error {
		_, pending, usageErr := recordGhostLinkUsageTx(tx, oldInbound, postSnapshot, true)
		if usageErr != nil {
			return usageErr
		}
		if len(pending) > 0 {
			codes := make([]string, 0, len(pending))
			for code := range pending {
				codes = append(codes, code)
			}
			sort.Strings(codes)
			return fmt.Errorf("GhostLink post-change usage is pending: %s", strings.Join(codes, ","))
		}
		if err := commit(tx); err != nil {
			return err
		}
		if err := reconcileGhostLinkUsageMappingsTx(tx, newInbound, preSnapshot, postSnapshot); err != nil {
			return err
		}
		return commitGhostLinkPolicyPlanTx(tx, targetPlan)
	}); err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		rollbackErr := operations.RollbackGhostLinkUserConfig(rollbackCtx, operation.OperationID)
		return false, errors.Join(
			fmt.Errorf("commit GhostLink client database change: %w", err),
			rollbackErr,
		)
	}
	confirmCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := operations.ConfirmGhostLinkUserConfig(confirmCtx, operation.OperationID); err != nil {
		return false, fmt.Errorf(
			"GhostLink database and runtime are committed but operation %s confirmation is pending recovery: %w",
			operation.OperationID,
			err,
		)
	}
	return false, nil
}

func checkGhostLinkSingletonTx(db *gorm.DB, ignoreID int) error {
	query := db.Model(&model.Inbound{}).Where("protocol = ?", model.GhostLink)
	if ignoreID > 0 {
		query = query.Where("id != ?", ignoreID)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return common.NewError("a GhostLink inbound already exists; this version supports exactly one GhostLink inbound per panel/node")
	}
	return nil
}

// GhostLinkDesiredState returns the single local desired configuration used by
// the lifecycle controller. The state-database path is supplied by the
// controller and is never accepted from an untrusted Inbound field.
func (s *InboundService) GhostLinkDesiredState(stateDBFile string) (ghostlink.DesiredState, bool, error) {
	var inbounds []*model.Inbound
	if err := database.GetDB().Where("protocol = ? AND node_id IS NULL", model.GhostLink).Order("id ASC").Find(&inbounds).Error; err != nil {
		return ghostlink.DesiredState{}, false, err
	}
	if len(inbounds) == 0 {
		return ghostlink.DesiredState{}, false, nil
	}
	if len(inbounds) != 1 {
		return ghostlink.DesiredState{}, false, common.NewError("multiple GhostLink inbounds found; refusing to build an ambiguous single-process desired state")
	}
	state, err := ghostlink.BuildDesiredState(inbounds[0], stateDBFile)
	if err != nil {
		return ghostlink.DesiredState{}, true, err
	}
	return state, true, nil
}
