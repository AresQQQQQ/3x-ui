package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/xray"
)

func seedGhostLinkUsageInbound(t *testing.T) (*model.Inbound, *InboundService) {
	t.Helper()
	setupConflictDB(t)
	svc := &InboundService{}
	inbound, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   false,
		Listen:   "127.0.0.1",
		Port:     38445,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return inbound, svc
}

func ghostLinkUsageSnapshot(storeID string, upload, download uint64) ghostlink.UsageSnapshot {
	return ghostlink.UsageSnapshot{
		StoreID: storeID,
		Users: []ghostlink.UsageUser{{
			ClientID:      "alice-device",
			UploadBytes:   upload,
			DownloadBytes: download,
		}},
		Source: "test",
	}
}

func ghostLinkUsageCounters(t *testing.T, inboundID int) (xray.ClientTraffic, model.Inbound) {
	t.Helper()
	db := database.GetDB()
	var traffic xray.ClientTraffic
	if err := db.Where("email = ?", "alice@example.test").First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	var inbound model.Inbound
	if err := db.First(&inbound, inboundID).Error; err != nil {
		t.Fatal(err)
	}
	return traffic, inbound
}

func TestGhostLinkUsageBaselineIncrementAndDuplicateSnapshot(t *testing.T) {
	inbound, svc := seedGhostLinkUsageInbound(t)

	if _, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 100, 200)); err != nil {
		t.Fatal(err)
	}
	traffic, storedInbound := ghostLinkUsageCounters(t, inbound.Id)
	if traffic.Up != 0 || traffic.Down != 0 || storedInbound.Up != 0 || storedInbound.Down != 0 {
		t.Fatalf("first observation backfilled historical usage: traffic=%+v inbound=%+v", traffic, storedInbound)
	}

	result, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 150, 260))
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadDelta != 50 || result.DownloadDelta != 60 {
		t.Fatalf("usage delta = %#v", result)
	}
	traffic, storedInbound = ghostLinkUsageCounters(t, inbound.Id)
	if traffic.Up != 50 || traffic.Down != 60 || storedInbound.Up != 50 || storedInbound.Down != 60 {
		t.Fatalf("usage ledger mismatch: traffic=%+v inbound=%+v", traffic, storedInbound)
	}

	result, err = svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 150, 260))
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadDelta != 0 || result.DownloadDelta != 0 {
		t.Fatalf("duplicate snapshot charged again: %#v", result)
	}
	traffic, storedInbound = ghostLinkUsageCounters(t, inbound.Id)
	if traffic.Up != 50 || traffic.Down != 60 || storedInbound.Up != 50 || storedInbound.Down != 60 {
		t.Fatalf("duplicate snapshot changed ledger: traffic=%+v inbound=%+v", traffic, storedInbound)
	}
}

func TestGhostLinkUsageRegressionAndStoreChangeFailClosed(t *testing.T) {
	inbound, svc := seedGhostLinkUsageInbound(t)
	if _, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 100, 200)); err != nil {
		t.Fatal(err)
	}

	_, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 90, 210))
	var pending *GhostLinkUsagePendingError
	if !errors.As(err, &pending) || !strings.Contains(pending.Codes, "cumulative_counter_regressed") {
		t.Fatalf("regression error = %v", err)
	}
	_, err = svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:two", 120, 220))
	if !errors.As(err, &pending) || !strings.Contains(pending.Codes, "store_epoch_changed") {
		t.Fatalf("store change error = %v", err)
	}
	traffic, storedInbound := ghostLinkUsageCounters(t, inbound.Id)
	if traffic.Up != 0 || traffic.Down != 0 || storedInbound.Up != 0 || storedInbound.Down != 0 {
		t.Fatalf("regression charged traffic: traffic=%+v inbound=%+v", traffic, storedInbound)
	}
}

func TestGhostLinkUsageRejectsSharedEmailIdentity(t *testing.T) {
	inbound, svc := seedGhostLinkUsageInbound(t)
	if _, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 100, 200)); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	other := &model.Inbound{UserId: 1, Enable: true, Protocol: model.VLESS, Tag: "xray-shared-email", Settings: "{}"}
	if err := db.Create(other).Error; err != nil {
		t.Fatal(err)
	}
	var client model.ClientRecord
	if err := db.Where("email = ?", "alice@example.test").First(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: other.Id}).Error; err != nil {
		t.Fatal(err)
	}

	_, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 150, 260))
	var pending *GhostLinkUsagePendingError
	if !errors.As(err, &pending) || !strings.Contains(pending.Codes, "identity_conflict_or_missing") {
		t.Fatalf("shared identity error = %v", err)
	}
	traffic, _ := ghostLinkUsageCounters(t, inbound.Id)
	if traffic.Up != 0 || traffic.Down != 0 {
		t.Fatalf("shared identity charged traffic: %+v", traffic)
	}
}

func TestGhostLinkUsageDBFailureRetriesSameSnapshotExactlyOnce(t *testing.T) {
	inbound, svc := seedGhostLinkUsageInbound(t)
	if _, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 100, 200)); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	if err := db.Exec(`CREATE TRIGGER round5_fail_ghostlink_usage
		BEFORE UPDATE OF up ON client_traffics
		WHEN OLD.email = 'alice@example.test'
		BEGIN
			SELECT RAISE(ABORT, 'injected usage ledger failure');
		END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 180, 260)); err == nil {
		t.Fatal("injected database failure did not fail usage accounting")
	}
	if err := db.Exec("DROP TRIGGER round5_fail_ghostlink_usage").Error; err != nil {
		t.Fatal(err)
	}

	result, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, ghostLinkUsageSnapshot("store:one", 180, 260))
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadDelta != 80 || result.DownloadDelta != 60 {
		t.Fatalf("retry delta = %#v", result)
	}
	traffic, storedInbound := ghostLinkUsageCounters(t, inbound.Id)
	if traffic.Up != 80 || traffic.Down != 60 || storedInbound.Up != 80 || storedInbound.Down != 60 {
		t.Fatalf("retry charged wrong ledger: traffic=%+v inbound=%+v", traffic, storedInbound)
	}
}

func TestGhostLinkIsExcludedFromXrayTrafficDisableLifecycle(t *testing.T) {
	inbound, _ := seedGhostLinkUsageInbound(t)
	db := database.GetDB()
	if err := db.Model(&model.Inbound{}).Where("id = ?", inbound.Id).
		Updates(map[string]any{"enable": true, "total": 1, "up": 2, "down": 0}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", "alice@example.test").
		Updates(map[string]any{
			"enable": true, "total": 1, "up": 2, "down": 0,
			"reset": 1, "expiry_time": time.Now().Add(-time.Hour).UnixMilli(),
		}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &InboundService{}
	if _, count, err := svc.autoRenewClients(db, newTrafficMutationBatch()); err != nil || count != 0 {
		t.Fatalf("autoRenewClients count=%d err=%v", count, err)
	}
	if _, count, _, err := svc.disableInvalidClients(db, newTrafficMutationBatch()); err != nil || count != 0 {
		t.Fatalf("disableInvalidClients count=%d err=%v", count, err)
	}
	if _, count, err := svc.disableInvalidInbounds(db, newTrafficMutationBatch()); err != nil || count != 0 {
		t.Fatalf("disableInvalidInbounds count=%d err=%v", count, err)
	}
	var traffic xray.ClientTraffic
	if err := db.Where("email = ?", "alice@example.test").First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if !traffic.Enable {
		t.Fatal("Xray traffic lifecycle disabled a GhostLink client")
	}
	if traffic.Up != 2 || traffic.Down != 0 {
		t.Fatalf("Xray traffic lifecycle reset a GhostLink client: %+v", traffic)
	}
	var stored model.Inbound
	if err := db.First(&stored, inbound.Id).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.Enable {
		t.Fatal("Xray traffic lifecycle disabled the GhostLink inbound")
	}
}
