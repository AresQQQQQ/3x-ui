package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/xray"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ghostLinkPolicyOK           = "ok"
	ghostLinkPolicyManualOff    = "manual_disabled"
	ghostLinkPolicyExpired      = "expired"
	ghostLinkPolicyQuotaReached = "quota_exhausted"
	ghostLinkPolicyPendingUsage = "pending_usage"
)

type ghostLinkPolicyRuntime interface {
	ReconcileGhostLinkWithPolicies(context.Context, *model.Inbound, map[string]ghostlink.RuntimePolicy) (ghostlink.Status, error)
	GhostLinkStatusWithPolicies(context.Context, *model.Inbound, map[string]ghostlink.RuntimePolicy) (ghostlink.Status, error)
	ApplyGhostLinkUserPolicyConfig(
		context.Context,
		*model.Inbound,
		*model.Inbound,
		map[string]ghostlink.RuntimePolicy,
		map[string]ghostlink.RuntimePolicy,
		string,
		string,
	) (ghostlink.UserOperation, error)
}

type ghostLinkPolicyPlan struct {
	rows        map[string]*model.GhostLinkUsageWatermark
	policies    map[string]ghostlink.RuntimePolicy
	oldPolicies map[string]ghostlink.RuntimePolicy
	resetIDs    map[string]bool
	pending     bool
}

func currentGhostLinkRuntimePolicies(inbound *model.Inbound) (map[string]ghostlink.RuntimePolicy, error) {
	if inbound == nil || inbound.Protocol != model.GhostLink {
		return nil, errors.New("not a GhostLink inbound")
	}
	settings, err := ghostlink.ParseSettings(inbound.Settings)
	if err != nil {
		return nil, err
	}
	rows, err := loadGhostLinkUsageWatermarks(database.GetDB(), inbound.Id)
	if err != nil {
		return nil, err
	}
	policies := make(map[string]ghostlink.RuntimePolicy, len(settings.Clients))
	for _, client := range settings.Clients {
		policy := ghostlink.RuntimePolicy{Enabled: client.Enable}
		if row := rows[client.ID]; row != nil && row.PolicyVersion > 0 {
			policy = ghostlink.RuntimePolicy{
				Enabled:            row.EffectiveEnabled,
				MaxTrafficBytes:    uint64(maxInt64(row.MaxTrafficBytes, 0)),
				ExpiresAtUnixMilli: row.ExpiryTime,
			}
		}
		policies[client.ID] = policy
	}
	return policies, nil
}

func (s *InboundService) ghostLinkPolicyRuntime(inbound *model.Inbound) (ghostLinkPolicyRuntime, error) {
	rt, err := s.runtimeFor(inbound)
	if err != nil {
		return nil, err
	}
	policyRuntime, ok := rt.(ghostLinkPolicyRuntime)
	if !ok {
		return nil, errors.New("local runtime does not provide GhostLink runtime policy updates")
	}
	return policyRuntime, nil
}

func ghostLinkPolicyTimeLocation() *time.Location {
	loc, err := (&SettingService{}).GetTimeLocation()
	if err != nil || loc == nil {
		return time.UTC
	}
	return loc
}

func buildGhostLinkPolicyPlan(
	inbound *model.Inbound,
	snapshot ghostlink.UsageSnapshot,
	rows map[string]*model.GhostLinkUsageWatermark,
	traffic map[string]xray.ClientTraffic,
	now time.Time,
	loc *time.Location,
	forceReset map[string]bool,
) (ghostLinkPolicyPlan, error) {
	settings, err := ghostlink.ParseSettings(inbound.Settings)
	if err != nil {
		return ghostLinkPolicyPlan{}, err
	}
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)
	nowMs := now.UnixMilli()
	plan := ghostLinkPolicyPlan{
		rows:        make(map[string]*model.GhostLinkUsageWatermark, len(settings.Clients)),
		policies:    make(map[string]ghostlink.RuntimePolicy, len(settings.Clients)),
		oldPolicies: make(map[string]ghostlink.RuntimePolicy, len(settings.Clients)),
		resetIDs:    make(map[string]bool),
	}
	usageUsers := make(map[string]ghostlink.UsageUser, len(snapshot.Users))
	for _, user := range snapshot.Users {
		usageUsers[user.ClientID] = user
	}

	for _, client := range settings.Clients {
		row := rows[client.ID]
		current := model.GhostLinkUsageWatermark{}
		if row != nil {
			current = *row
			enabled := client.Enable
			maxTraffic := int64(0)
			if row.PolicyVersion > 0 {
				enabled = row.EffectiveEnabled
				maxTraffic = row.MaxTrafficBytes
			}
			plan.oldPolicies[client.ID] = ghostlink.RuntimePolicy{
				Enabled:            enabled,
				MaxTrafficBytes:    uint64(maxInt64(maxTraffic, 0)),
				ExpiresAtUnixMilli: current.ExpiryTime,
			}
		} else {
			plan.oldPolicies[client.ID] = ghostlink.RuntimePolicy{
				Enabled:            client.Enable,
				MaxTrafficBytes:    0,
				ExpiresAtUnixMilli: client.ExpiryTime,
			}
		}

		usage, usagePresent := usageUsers[client.ID]
		if !usagePresent && row == nil && snapshot.StoreID == "uninitialized" {
			usagePresent = true
		}
		usagePending := row != nil && row.State == "pending"
		if client.Enable && !usagePresent && snapshot.Source != "sqlite-uninitialized" {
			usagePending = true
		}
		if usage.PersistenceFailed {
			usagePending = true
		}

		panelUsed, ok := ghostLinkPanelUsage(traffic[strings.ToLower(client.Email)])
		if !ok {
			usagePending = true
			panelUsed = 0
		}
		coreUsed, ok := ghostLinkCommittedTotal(usage)
		if !ok {
			usagePending = true
			coreUsed = 0
		}

		schedule := client.TrafficReset
		if schedule == "" {
			schedule = "never"
		}
		scheduleChanged := current.TrafficReset != schedule || current.TrafficResetDay != client.TrafficResetDay
		nextReset := current.NextResetAt
		if scheduleChanged || nextReset == 0 {
			nextReset = ghostLinkNextResetAt(now, schedule, client.TrafficResetDay, loc)
		}
		resetDue := forceReset[client.ID] || (!scheduleChanged && nextReset > 0 && nowMs >= nextReset)
		if resetDue && usagePending {
			resetDue = false
		}
		if resetDue {
			plan.resetIDs[client.ID] = true
			panelUsed = 0
			nextReset = ghostLinkNextResetAt(now, schedule, client.TrafficResetDay, loc)
			current.LastResetAt = nowMs
		}

		quota := maxInt64(client.TotalGB, 0)
		quotaChanged := current.QuotaBytes != quota
		maxTraffic := current.MaxTrafficBytes
		switch {
		case quota == 0:
			maxTraffic = 0
		case resetDue || quotaChanged || maxTraffic <= 0:
			remaining := quota - panelUsed
			if remaining < 0 {
				remaining = 0
			}
			if coreUsed > math.MaxInt64-remaining {
				return ghostLinkPolicyPlan{}, errors.New("GhostLink quota ceiling exceeds SQLite INTEGER range")
			}
			maxTraffic = coreUsed + remaining
		}

		policyState := ghostLinkPolicyOK
		policyError := ""
		switch {
		case !client.Enable:
			policyState = ghostLinkPolicyManualOff
		case usagePending:
			policyState = ghostLinkPolicyPendingUsage
			policyError = "usage_pending"
		case client.ExpiryTime > 0 && nowMs >= client.ExpiryTime:
			policyState = ghostLinkPolicyExpired
		case quota > 0 && panelUsed >= quota:
			policyState = ghostLinkPolicyQuotaReached
		}
		effective := client.Enable && policyState == ghostLinkPolicyOK
		if policyState == ghostLinkPolicyPendingUsage {
			plan.pending = true
		}

		current.InboundId = inbound.Id
		current.ClientId = client.ID
		current.Email = client.Email
		if current.StoreId == "" {
			current.StoreId = snapshot.StoreID
		}
		current.ManualEnabled = client.Enable
		current.EffectiveEnabled = effective
		current.PolicyVersion = 1
		current.PolicyState = policyState
		current.PolicyErrorCode = policyError
		current.QuotaBytes = quota
		current.ExpiryTime = client.ExpiryTime
		current.MaxTrafficBytes = maxTraffic
		current.TrafficReset = schedule
		current.TrafficResetDay = client.TrafficResetDay
		current.NextResetAt = nextReset
		plan.rows[client.ID] = &current
		plan.policies[client.ID] = ghostlink.RuntimePolicy{
			Enabled:            effective,
			MaxTrafficBytes:    uint64(maxInt64(maxTraffic, 0)),
			ExpiresAtUnixMilli: client.ExpiryTime,
		}
	}
	return plan, nil
}

func ghostLinkPanelUsage(traffic xray.ClientTraffic) (int64, bool) {
	if traffic.Up < 0 || traffic.Down < 0 {
		return 0, false
	}
	if traffic.Up > math.MaxInt64-traffic.Down {
		return math.MaxInt64, true
	}
	return traffic.Up + traffic.Down, true
}

func ghostLinkCommittedTotal(user ghostlink.UsageUser) (int64, bool) {
	if user.UploadBytes > math.MaxInt64 || user.DownloadBytes > math.MaxInt64 {
		return 0, false
	}
	upload := int64(user.UploadBytes)
	download := int64(user.DownloadBytes)
	if upload > math.MaxInt64-download {
		return math.MaxInt64, true
	}
	return upload + download, true
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func ghostLinkNextResetAt(now time.Time, cycle string, resetDay int, loc *time.Location) int64 {
	now = now.In(loc)
	switch cycle {
	case "hourly":
		return time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, loc).Add(time.Hour).UnixMilli()
	case "daily":
		return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc).UnixMilli()
	case "weekly":
		daysUntilSunday := (7 - int(now.Weekday())) % 7
		if daysUntilSunday == 0 {
			daysUntilSunday = 7
		}
		return time.Date(now.Year(), now.Month(), now.Day()+daysUntilSunday, 0, 0, 0, 0, loc).UnixMilli()
	case "monthly":
		if resetDay < 1 {
			resetDay = 1
		}
		next := ghostLinkMonthlyReset(now, resetDay, loc)
		if !now.Before(next) {
			next = ghostLinkMonthlyReset(now.AddDate(0, 1, 0), resetDay, loc)
		}
		return next.UnixMilli()
	default:
		return 0
	}
}

func ghostLinkMonthlyReset(now time.Time, resetDay int, loc *time.Location) time.Time {
	lastDay := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, loc).Day()
	day := min(resetDay, lastDay)
	return time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, loc)
}

func (s *InboundService) loadGhostLinkPolicyPlan(
	ctx context.Context,
	inbound *model.Inbound,
	snapshot ghostlink.UsageSnapshot,
	forceReset map[string]bool,
) (ghostLinkPolicyPlan, error) {
	settings, err := ghostlink.ParseSettings(inbound.Settings)
	if err != nil {
		return ghostLinkPolicyPlan{}, err
	}
	emails := make(map[string]struct{}, len(settings.Clients))
	for _, client := range settings.Clients {
		emails[strings.ToLower(client.Email)] = struct{}{}
	}
	db := database.GetDB()
	rows, err := loadGhostLinkUsageWatermarks(db, inbound.Id)
	if err != nil {
		return ghostLinkPolicyPlan{}, err
	}
	traffic, err := loadGhostLinkClientTraffic(db, emails)
	if err != nil {
		return ghostLinkPolicyPlan{}, err
	}
	return buildGhostLinkPolicyPlan(inbound, snapshot, rows, traffic, time.Now(), ghostLinkPolicyTimeLocation(), forceReset)
}

func commitGhostLinkPolicyPlanTx(tx *gorm.DB, plan ghostLinkPolicyPlan) error {
	ids := make([]string, 0, len(plan.rows))
	for id := range plan.rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		row := plan.rows[id]
		if plan.resetIDs[id] {
			result := tx.Model(&xray.ClientTraffic{}).
				Where("LOWER(email) = ?", strings.ToLower(row.Email)).
				Updates(map[string]any{"up": 0, "down": 0})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("GhostLink quota reset could not find client traffic row")
			}
		}
	}
	return upsertGhostLinkPolicyRowsTx(tx, plan)
}

func upsertGhostLinkPolicyRowsTx(tx *gorm.DB, plan ghostLinkPolicyPlan) error {
	ids := make([]string, 0, len(plan.rows))
	for id := range plan.rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		row := plan.rows[id]
		values := map[string]any{
			"inbound_id":        row.InboundId,
			"client_id":         row.ClientId,
			"email":             row.Email,
			"manual_enabled":    row.ManualEnabled,
			"effective_enabled": row.EffectiveEnabled,
			"policy_version":    row.PolicyVersion,
			"policy_state":      row.PolicyState,
			"policy_error_code": row.PolicyErrorCode,
			"quota_bytes":       row.QuotaBytes,
			"expiry_time":       row.ExpiryTime,
			"max_traffic_bytes": row.MaxTrafficBytes,
			"traffic_reset":     row.TrafficReset,
			"traffic_reset_day": row.TrafficResetDay,
			"next_reset_at":     row.NextResetAt,
			"last_reset_at":     row.LastResetAt,
		}
		if err := tx.Model(&model.GhostLinkUsageWatermark{}).
			Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "inbound_id"}, {Name: "client_id"}},
				DoUpdates: clause.Assignments(values),
			}).Create(map[string]any{
			"inbound_id":        row.InboundId,
			"client_id":         row.ClientId,
			"email":             row.Email,
			"store_id":          row.StoreId,
			"upload_bytes":      0,
			"download_bytes":    0,
			"state":             "baseline",
			"error_code":        "",
			"manual_enabled":    row.ManualEnabled,
			"effective_enabled": row.EffectiveEnabled,
			"policy_version":    row.PolicyVersion,
			"policy_state":      row.PolicyState,
			"policy_error_code": row.PolicyErrorCode,
			"quota_bytes":       row.QuotaBytes,
			"expiry_time":       row.ExpiryTime,
			"max_traffic_bytes": row.MaxTrafficBytes,
			"traffic_reset":     row.TrafficReset,
			"traffic_reset_day": row.TrafficResetDay,
			"next_reset_at":     row.NextResetAt,
			"last_reset_at":     row.LastResetAt,
			"last_snapshot_at":  row.LastSnapshotAt,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func ghostLinkPoliciesEqual(a, b map[string]ghostlink.RuntimePolicy) bool {
	if len(a) != len(b) {
		return false
	}
	for id, left := range a {
		right, ok := b[id]
		if !ok || left != right {
			return false
		}
	}
	return true
}

func (s *InboundService) reconcileGhostLinkPolicySnapshotLocked(
	ctx context.Context,
	inbound *model.Inbound,
	snapshot ghostlink.UsageSnapshot,
	forceReset map[string]bool,
) error {
	plan, err := s.loadGhostLinkPolicyPlan(ctx, inbound, snapshot, forceReset)
	if err != nil {
		return err
	}
	if operations, opErr := s.ghostLinkUserOperations(inbound); opErr == nil {
		desiredRevision, revisionErr := ghostlink.UserRevisionForInboundWithPolicies(inbound, plan.oldPolicies)
		if revisionErr != nil {
			return revisionErr
		}
		if pending, exists, pendingErr := operations.PendingGhostLinkUserConfig(ctx); pendingErr != nil {
			return pendingErr
		} else if exists {
			if _, resolveErr := operations.ResolvePendingGhostLinkUserConfig(ctx, desiredRevision); resolveErr != nil {
				return fmt.Errorf("recover GhostLink policy operation %s: %w", pending.OperationID, resolveErr)
			}
		}
	}
	policyRuntime, err := s.ghostLinkPolicyRuntime(inbound)
	if err != nil {
		return err
	}
	operations, err := s.ghostLinkUserOperations(inbound)
	if err != nil {
		return err
	}
	targetStatus, statusErr := policyRuntime.GhostLinkStatusWithPolicies(ctx, inbound, plan.policies)
	if statusErr != nil {
		return statusErr
	}
	runtimeMatches := false
	if targetStatus.Expected {
		runtimeMatches = targetStatus.Ready &&
			targetStatus.ActiveRevision == targetStatus.DesiredRevision &&
			targetStatus.RuntimeRevision == targetStatus.DesiredUserRevision
	} else {
		runtimeMatches = !targetStatus.Running &&
			targetStatus.ActiveRevision == targetStatus.DesiredRevision
	}
	policyChanged := !ghostLinkPoliciesEqual(plan.oldPolicies, plan.policies)
	if policyChanged || !runtimeMatches {
		if targetStatus.Expected && !targetStatus.Running {
			if err := runSerializedTx(func(tx *gorm.DB) error {
				return commitGhostLinkPolicyPlanTx(tx, plan)
			}); err != nil {
				return err
			}
			if _, err := policyRuntime.ReconcileGhostLinkWithPolicies(ctx, inbound, plan.policies); err != nil {
				return fmt.Errorf("start GhostLink with current business policy: %w", err)
			}
			if plan.pending {
				return &GhostLinkUsagePendingError{Count: 1, Codes: "usage_pending"}
			}
			return nil
		}
		oldStatus, oldErr := policyRuntime.GhostLinkStatusWithPolicies(ctx, inbound, plan.oldPolicies)
		if oldErr != nil && oldStatus.Running {
			return oldErr
		}
		requestID, idErr := ghostlink.NewUserOperationID()
		if idErr != nil {
			return idErr
		}
		operation, applyErr := policyRuntime.ApplyGhostLinkUserPolicyConfig(
			ctx,
			inbound,
			inbound,
			plan.oldPolicies,
			plan.policies,
			oldStatus.RuntimeRevision,
			requestID,
		)
		if applyErr != nil {
			return fmt.Errorf("apply GhostLink runtime policy: %w", applyErr)
		}
		commitErr := runSerializedTx(func(tx *gorm.DB) error {
			return commitGhostLinkPolicyPlanTx(tx, plan)
		})
		if commitErr != nil {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			rollbackErr := operations.RollbackGhostLinkUserConfig(rollbackCtx, operation.OperationID)
			cancel()
			return errors.Join(commitErr, rollbackErr)
		}
		confirmCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		confirmErr := operations.ConfirmGhostLinkUserConfig(confirmCtx, operation.OperationID)
		cancel()
		if confirmErr != nil {
			return fmt.Errorf("GhostLink policy commit is pending confirmation recovery: %w", confirmErr)
		}
	} else {
		if err := submitTrafficWrite(func() error {
			return database.GetDB().Transaction(func(tx *gorm.DB) error {
				return commitGhostLinkPolicyPlanTx(tx, plan)
			})
		}); err != nil {
			return err
		}
	}
	if plan.pending {
		return &GhostLinkUsagePendingError{
			Count: 1,
			Codes: "usage_pending",
		}
	}
	return nil
}

func (s *InboundService) ResetGhostLinkTrafficByEmail(ctx context.Context, email string) error {
	ghostLinkUserOperationMu.Lock()
	defer ghostLinkUserOperationMu.Unlock()

	inbound, found, err := s.localGhostLinkInbound()
	if err != nil || !found {
		return errors.Join(err, errors.New("GhostLink inbound is not configured"))
	}
	settings, err := ghostlink.ParseSettings(inbound.Settings)
	if err != nil {
		return err
	}
	clientID := ""
	for _, client := range settings.Clients {
		if strings.EqualFold(client.Email, email) {
			clientID = client.ID
			break
		}
	}
	if clientID == "" {
		return errors.New("GhostLink client email is not present in the inbound")
	}
	result, snapshot, usageErr := s.collectGhostLinkUsageSnapshotLocked(ctx, inbound, inbound)
	if usageErr != nil {
		return usageErr
	}
	if result.Pending > 0 {
		return errors.New("GhostLink usage is pending; reset is blocked until reconciliation completes")
	}
	return s.reconcileGhostLinkPolicySnapshotLocked(
		ctx,
		inbound,
		snapshot,
		map[string]bool{clientID: true},
	)
}
