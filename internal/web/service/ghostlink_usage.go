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
)

type ghostLinkUsageRuntime interface {
	GhostLinkUsageSnapshot(context.Context, *model.Inbound) (ghostlink.UsageSnapshot, error)
}

type GhostLinkUsageResult struct {
	InboundID     int
	Source        string
	UsersSeen     int
	UsersCharged  int
	Baselines     int
	UploadDelta   uint64
	DownloadDelta uint64
	Pending       int
}

type GhostLinkUsagePendingError struct {
	Count int
	Codes string
}

func (e *GhostLinkUsagePendingError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("GhostLink usage accounting has %d pending user(s): %s", e.Count, e.Codes)
}

func (s *InboundService) CollectGhostLinkUsage(ctx context.Context) (GhostLinkUsageResult, error) {
	ghostLinkUserOperationMu.Lock()
	defer ghostLinkUserOperationMu.Unlock()
	return s.runGhostLinkUsagePolicyCycleLocked(ctx)
}

func (s *InboundService) runGhostLinkUsagePolicyCycleLocked(ctx context.Context) (GhostLinkUsageResult, error) {
	inbound, found, err := s.localGhostLinkInbound()
	if err != nil {
		return GhostLinkUsageResult{}, err
	}
	if !found {
		_, reconcileErr := s.reconcileGhostLinkLocked(ctx)
		return GhostLinkUsageResult{}, reconcileErr
	}
	result, snapshot, usageErr := s.collectGhostLinkUsageSnapshotLocked(ctx, inbound, inbound)
	if usageErr != nil {
		var pending *GhostLinkUsagePendingError
		if !errors.As(usageErr, &pending) {
			return result, usageErr
		}
	}
	policyErr := s.reconcileGhostLinkPolicySnapshotLocked(ctx, inbound, snapshot, nil)
	_, reconcileErr := s.reconcileGhostLinkLocked(ctx)
	return result, errors.Join(usageErr, policyErr, reconcileErr)
}

func (s *InboundService) collectGhostLinkUsageLocked(
	ctx context.Context,
	probeInbound *model.Inbound,
	mappingInbound *model.Inbound,
) (GhostLinkUsageResult, error) {
	result, _, err := s.collectGhostLinkUsageSnapshotLocked(ctx, probeInbound, mappingInbound)
	return result, err
}

func (s *InboundService) collectGhostLinkUsageSnapshotLocked(
	ctx context.Context,
	probeInbound *model.Inbound,
	mappingInbound *model.Inbound,
) (GhostLinkUsageResult, ghostlink.UsageSnapshot, error) {
	rt, err := s.runtimeFor(probeInbound)
	if err != nil {
		return GhostLinkUsageResult{}, ghostlink.UsageSnapshot{}, err
	}
	usageRuntime, ok := rt.(ghostLinkUsageRuntime)
	if !ok {
		return GhostLinkUsageResult{}, ghostlink.UsageSnapshot{}, fmt.Errorf("local runtime does not provide GhostLink usage snapshots")
	}
	snapshot, err := usageRuntime.GhostLinkUsageSnapshot(ctx, probeInbound)
	if err != nil {
		return GhostLinkUsageResult{}, ghostlink.UsageSnapshot{}, err
	}
	result, err := s.recordGhostLinkUsageSnapshot(ctx, mappingInbound, snapshot)
	return result, snapshot, err
}

func (s *InboundService) recordGhostLinkUsageSnapshot(
	ctx context.Context,
	inbound *model.Inbound,
	snapshot ghostlink.UsageSnapshot,
) (GhostLinkUsageResult, error) {
	if inbound == nil || inbound.Protocol != model.GhostLink {
		return GhostLinkUsageResult{}, fmt.Errorf("not a GhostLink inbound")
	}
	result := GhostLinkUsageResult{
		InboundID: inbound.Id,
		Source:    snapshot.Source,
		UsersSeen: len(snapshot.Users),
	}
	pending := make(map[string]int)
	err := submitTrafficWrite(func() error {
		return database.GetDB().Transaction(func(tx *gorm.DB) error {
			var inner error
			result, pending, inner = recordGhostLinkUsageTx(tx, inbound, snapshot, false)
			return inner
		})
	})
	if err != nil {
		return result, err
	}
	if len(pending) > 0 {
		codes := make([]string, 0, len(pending))
		for code, count := range pending {
			codes = append(codes, fmt.Sprintf("%s=%d", code, count))
			result.Pending += count
		}
		sort.Strings(codes)
		return result, &GhostLinkUsagePendingError{
			Count: result.Pending,
			Codes: strings.Join(codes, ","),
		}
	}
	return result, nil
}

func recordGhostLinkUsageTx(
	tx *gorm.DB,
	inbound *model.Inbound,
	snapshot ghostlink.UsageSnapshot,
	ignoreUnmapped bool,
) (GhostLinkUsageResult, map[string]int, error) {
	result := GhostLinkUsageResult{
		InboundID: inbound.Id,
		Source:    snapshot.Source,
		UsersSeen: len(snapshot.Users),
	}
	pending := make(map[string]int)
	if len(snapshot.Users) == 0 {
		return result, pending, nil
	}
	if snapshot.StoreID == "" {
		return result, pending, fmt.Errorf("GhostLink usage snapshot has no store identity")
	}
	identities, err := ghostlink.ClientIdentityMap(inbound)
	if err != nil {
		return result, pending, err
	}
	watermarks, err := loadGhostLinkUsageWatermarks(tx, inbound.Id)
	if err != nil {
		return result, pending, err
	}

	emails := make(map[string]struct{}, len(snapshot.Users)+len(watermarks))
	for id, email := range identities {
		if _, present := snapshotUser(snapshot.Users, id); present {
			emails[strings.ToLower(email)] = struct{}{}
		}
	}
	for _, user := range snapshot.Users {
		if watermark := watermarks[user.ClientID]; watermark != nil {
			emails[strings.ToLower(watermark.Email)] = struct{}{}
		}
	}
	associations, err := loadGhostLinkEmailAssociations(tx, emails)
	if err != nil {
		return result, pending, err
	}
	ledgerRows, err := loadGhostLinkClientTraffic(tx, emails)
	if err != nil {
		return result, pending, err
	}

	now := time.Now().UnixMilli()
	markPending := func(watermark *model.GhostLinkUsageWatermark, code string) error {
		pending[code]++
		if watermark == nil {
			return nil
		}
		watermark.State = "pending"
		watermark.ErrorCode = code
		watermark.LastSnapshotAt = now
		return tx.Save(watermark).Error
	}

	for _, user := range snapshot.Users {
		if user.ClientID == "" {
			pending["missing_client_id"]++
			continue
		}
		if user.UploadBytes > math.MaxInt64 || user.DownloadBytes > math.MaxInt64 {
			if err := markPending(watermarks[user.ClientID], "counter_out_of_range"); err != nil {
				return result, pending, err
			}
			continue
		}
		email := identities[user.ClientID]
		watermark := watermarks[user.ClientID]
		if email == "" && watermark != nil {
			email = watermark.Email
		}
		if email == "" {
			if ignoreUnmapped && watermark == nil {
				continue
			}
			pending["missing_email_mapping"]++
			continue
		}
		if !ghostLinkEmailIdentitySafe(associations, inbound.Id, email, watermark != nil) {
			if err := markPending(watermark, "identity_conflict_or_missing"); err != nil {
				return result, pending, err
			}
			continue
		}
		if user.PersistenceFailed {
			if err := markPending(watermark, "core_persistence_failed"); err != nil {
				return result, pending, err
			}
			continue
		}

		if watermark == nil {
			watermark = &model.GhostLinkUsageWatermark{
				InboundId:      inbound.Id,
				ClientId:       user.ClientID,
				Email:          email,
				StoreId:        snapshot.StoreID,
				UploadBytes:    int64(user.UploadBytes),
				DownloadBytes:  int64(user.DownloadBytes),
				State:          "baseline",
				LastSnapshotAt: now,
			}
			if err := tx.Create(watermark).Error; err != nil {
				return result, pending, err
			}
			watermarks[user.ClientID] = watermark
			result.Baselines++
			continue
		}
		if watermark.StoreId == "uninitialized" &&
			snapshot.StoreID != "" && snapshot.StoreID != "uninitialized" {
			// The first policy cycle may run before systemd creates the Core
			// state DB. Adopt the first real store ID only while the watermark
			// has no authoritative epoch yet.
			watermark.StoreId = snapshot.StoreID
		}
		if watermark.StoreId != snapshot.StoreID {
			if err := markPending(watermark, "store_epoch_changed"); err != nil {
				return result, pending, err
			}
			continue
		}
		if watermark.UploadBytes < 0 || watermark.DownloadBytes < 0 {
			if err := markPending(watermark, "watermark_corrupt"); err != nil {
				return result, pending, err
			}
			continue
		}
		if user.UploadBytes < uint64(watermark.UploadBytes) ||
			user.DownloadBytes < uint64(watermark.DownloadBytes) {
			if err := markPending(watermark, "cumulative_counter_regressed"); err != nil {
				return result, pending, err
			}
			continue
		}
		uploadDelta := user.UploadBytes - uint64(watermark.UploadBytes)
		downloadDelta := user.DownloadBytes - uint64(watermark.DownloadBytes)
		if uploadDelta == 0 && downloadDelta == 0 {
			watermark.State = "ok"
			watermark.ErrorCode = ""
			watermark.LastSnapshotAt = now
			watermark.Email = email
			if err := tx.Save(watermark).Error; err != nil {
				return result, pending, err
			}
			continue
		}
		if _, ok := ledgerRows[strings.ToLower(email)]; !ok {
			if err := markPending(watermark, "client_traffic_missing"); err != nil {
				return result, pending, err
			}
			continue
		}
		if uploadDelta > math.MaxInt64 || downloadDelta > math.MaxInt64 {
			if err := markPending(watermark, "delta_out_of_range"); err != nil {
				return result, pending, err
			}
			continue
		}
		update := tx.Exec(
			fmt.Sprintf(
				`UPDATE client_traffics SET up = %s, down = %s, last_online = %s WHERE email = ?`,
				database.ClampedAddExpr("up"),
				database.ClampedAddExpr("down"),
				database.GreatestExpr("last_online", "?"),
			),
			int64(uploadDelta), int64(downloadDelta), now, email,
		)
		if update.Error != nil {
			return result, pending, update.Error
		}
		if update.RowsAffected == 0 {
			if err := markPending(watermark, "client_traffic_missing"); err != nil {
				return result, pending, err
			}
			continue
		}
		watermark.Email = email
		watermark.StoreId = snapshot.StoreID
		watermark.UploadBytes = int64(user.UploadBytes)
		watermark.DownloadBytes = int64(user.DownloadBytes)
		watermark.State = "ok"
		watermark.ErrorCode = ""
		watermark.LastSnapshotAt = now
		if err := tx.Save(watermark).Error; err != nil {
			return result, pending, err
		}
		result.UsersCharged++
		result.UploadDelta += uploadDelta
		result.DownloadDelta += downloadDelta
	}

	if result.UploadDelta > math.MaxInt64 || result.DownloadDelta > math.MaxInt64 {
		return result, pending, fmt.Errorf("GhostLink aggregate usage delta exceeds SQLite INTEGER range")
	}
	if result.UploadDelta > 0 || result.DownloadDelta > 0 {
		update := tx.Model(&model.Inbound{}).
			Where("id = ? AND protocol = ?", inbound.Id, model.GhostLink).
			Updates(map[string]any{
				"up":   gorm.Expr(database.ClampedAddExpr("up"), int64(result.UploadDelta)),
				"down": gorm.Expr(database.ClampedAddExpr("down"), int64(result.DownloadDelta)),
			})
		if update.Error != nil {
			return result, pending, update.Error
		}
		if update.RowsAffected == 0 {
			return result, pending, fmt.Errorf("GhostLink inbound disappeared during usage accounting")
		}
	}
	return result, pending, nil
}

func loadGhostLinkUsageWatermarks(tx *gorm.DB, inboundID int) (map[string]*model.GhostLinkUsageWatermark, error) {
	var rows []*model.GhostLinkUsageWatermark
	if err := tx.Where("inbound_id = ?", inboundID).Find(&rows).Error; err != nil {
		return nil, err
	}
	watermarks := make(map[string]*model.GhostLinkUsageWatermark, len(rows))
	for _, row := range rows {
		watermarks[row.ClientId] = row
	}
	return watermarks, nil
}

func snapshotUser(users []ghostlink.UsageUser, clientID string) (ghostlink.UsageUser, bool) {
	for _, user := range users {
		if user.ClientID == clientID {
			return user, true
		}
	}
	return ghostlink.UsageUser{}, false
}

type ghostLinkEmailAssociation struct {
	Email     string `gorm:"column:email"`
	InboundID int    `gorm:"column:inbound_id"`
	Protocol  string `gorm:"column:protocol"`
}

func loadGhostLinkEmailAssociations(tx *gorm.DB, emails map[string]struct{}) (map[string][]ghostLinkEmailAssociation, error) {
	out := make(map[string][]ghostLinkEmailAssociation, len(emails))
	if len(emails) == 0 {
		return out, nil
	}
	list := make([]string, 0, len(emails))
	for email := range emails {
		list = append(list, email)
	}
	rows := []ghostLinkEmailAssociation{}
	for _, batch := range chunkStrings(list, sqlInChunk) {
		var page []ghostLinkEmailAssociation
		if err := tx.Raw(`
			SELECT LOWER(clients.email) AS email, inbounds.id AS inbound_id, inbounds.protocol AS protocol
			FROM clients
			JOIN client_inbounds ON client_inbounds.client_id = clients.id
			JOIN inbounds ON inbounds.id = client_inbounds.inbound_id
			WHERE LOWER(clients.email) IN ?
		`, batch).Scan(&page).Error; err != nil {
			return nil, err
		}
		rows = append(rows, page...)
	}
	for _, row := range rows {
		out[row.Email] = append(out[row.Email], row)
	}
	return out, nil
}

func loadGhostLinkClientTraffic(tx *gorm.DB, emails map[string]struct{}) (map[string]xray.ClientTraffic, error) {
	out := make(map[string]xray.ClientTraffic, len(emails))
	list := make([]string, 0, len(emails))
	for email := range emails {
		list = append(list, email)
	}
	for _, batch := range chunkStrings(list, sqlInChunk) {
		var rows []xray.ClientTraffic
		if err := tx.Where("LOWER(email) IN ?", batch).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			out[strings.ToLower(row.Email)] = row
		}
	}
	return out, nil
}

func ghostLinkEmailIdentitySafe(
	associations map[string][]ghostLinkEmailAssociation,
	inboundID int,
	email string,
	trackedRemoved bool,
) bool {
	rows := associations[strings.ToLower(email)]
	if trackedRemoved && len(rows) == 0 {
		return true
	}
	if len(rows) != 1 {
		return false
	}
	return rows[0].InboundID == inboundID && model.Protocol(rows[0].Protocol) == model.GhostLink
}

func reconcileGhostLinkUsageMappingsTx(
	tx *gorm.DB,
	inbound *model.Inbound,
	preSnapshot ghostlink.UsageSnapshot,
	postSnapshot ghostlink.UsageSnapshot,
) error {
	if inbound == nil || inbound.Protocol != model.GhostLink {
		return fmt.Errorf("not a GhostLink inbound")
	}
	identities, err := ghostlink.ClientIdentityMap(inbound)
	if err != nil {
		return err
	}
	watermarks, err := loadGhostLinkUsageWatermarks(tx, inbound.Id)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for clientID, email := range identities {
		user, present := snapshotUser(postSnapshot.Users, clientID)
		if !present {
			continue
		}
		if user.UploadBytes > math.MaxInt64 || user.DownloadBytes > math.MaxInt64 {
			continue
		}
		watermark := watermarks[clientID]
		if watermark == nil {
			baselineUpload := uint64(0)
			baselineDownload := uint64(0)
			if preUser, exists := snapshotUser(preSnapshot.Users, clientID); exists {
				baselineUpload = preUser.UploadBytes
				baselineDownload = preUser.DownloadBytes
			}
			storeID := postSnapshot.StoreID
			if preSnapshot.StoreID != "" && preSnapshot.StoreID != "uninitialized" {
				storeID = preSnapshot.StoreID
			}
			watermark = &model.GhostLinkUsageWatermark{
				InboundId:      inbound.Id,
				ClientId:       clientID,
				Email:          email,
				StoreId:        storeID,
				UploadBytes:    int64(baselineUpload),
				DownloadBytes:  int64(baselineDownload),
				State:          "baseline",
				LastSnapshotAt: now,
			}
			if err := tx.Create(watermark).Error; err != nil {
				return err
			}
			continue
		}
		if watermark.Email != email {
			watermark.Email = email
			if err := tx.Save(watermark).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
