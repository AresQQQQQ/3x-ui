package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/xray"
	"gorm.io/gorm"
)

func ghostLinkPolicyFixture(t *testing.T, now time.Time) (*model.Inbound, ghostlink.UsageSnapshot, map[string]*model.GhostLinkUsageWatermark, map[string]xray.ClientTraffic) {
	t.Helper()
	var settings ghostlink.Settings
	if err := json.Unmarshal([]byte(ghostLinkSettingsJSON(t)), &settings); err != nil {
		t.Fatal(err)
	}
	settings.Clients[0].TotalGB = 1000
	settings.Clients[0].ExpiryTime = now.Add(time.Hour).UnixMilli()
	settings.Clients[0].TrafficReset = "daily"
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	inbound := &model.Inbound{Id: 1, Protocol: model.GhostLink, Settings: string(raw)}
	snapshot := ghostlink.UsageSnapshot{
		StoreID: "store:policy-test",
		Users: []ghostlink.UsageUser{{
			ClientID:      "alice-device",
			UploadBytes:   100,
			DownloadBytes: 200,
		}},
		Source: "test",
	}
	rows := map[string]*model.GhostLinkUsageWatermark{}
	traffic := map[string]xray.ClientTraffic{
		"alice@example.test": {Email: "alice@example.test", Up: 300, Down: 400},
	}
	return inbound, snapshot, rows, traffic
}

func TestGhostLinkPolicyQuotaUsesCoreBaselineAndPanelUsage(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	inbound, snapshot, rows, traffic := ghostLinkPolicyFixture(t, now)
	plan, err := buildGhostLinkPolicyPlan(inbound, snapshot, rows, traffic, now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.policies["alice-device"].Enabled {
		t.Fatal("eligible user was disabled")
	}
	if got := plan.policies["alice-device"].MaxTrafficBytes; got != 600 {
		t.Fatalf("runtime ceiling = %d, want 600 (300 committed + 300 remaining)", got)
	}

	traffic["alice@example.test"] = xray.ClientTraffic{Email: "alice@example.test", Up: 400, Down: 600}
	plan, err = buildGhostLinkPolicyPlan(inbound, snapshot, rows, traffic, now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.policies["alice-device"].Enabled {
		t.Fatal("quota-exhausted user stayed enabled")
	}
	if plan.rows["alice-device"].PolicyState != ghostLinkPolicyQuotaReached {
		t.Fatalf("policy state = %s", plan.rows["alice-device"].PolicyState)
	}

	inboundSettings := ghostlink.Settings{}
	if err := json.Unmarshal([]byte(inbound.Settings), &inboundSettings); err != nil {
		t.Fatal(err)
	}
	inboundSettings.Clients[0].TotalGB = 0
	raw, _ := json.Marshal(inboundSettings)
	inbound.Settings = string(raw)
	plan, err = buildGhostLinkPolicyPlan(inbound, snapshot, rows, traffic, now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.policies["alice-device"].Enabled || plan.policies["alice-device"].MaxTrafficBytes != 0 {
		t.Fatalf("zero quota policy = %+v", plan.policies["alice-device"])
	}
}

func TestGhostLinkPolicyExpiryAndManualDisableAreDistinct(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	inbound, snapshot, rows, traffic := ghostLinkPolicyFixture(t, now)
	var settings ghostlink.Settings
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		t.Fatal(err)
	}
	settings.Clients[0].ExpiryTime = now.UnixMilli()
	raw, _ := json.Marshal(settings)
	inbound.Settings = string(raw)
	plan, err := buildGhostLinkPolicyPlan(inbound, snapshot, rows, traffic, now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	row := plan.rows["alice-device"]
	if row.PolicyState != ghostLinkPolicyExpired || row.EffectiveEnabled || !row.ManualEnabled {
		t.Fatalf("expiry policy = %+v", row)
	}

	settings.Clients[0].ExpiryTime = 0
	settings.Clients[0].Enable = false
	raw, _ = json.Marshal(settings)
	inbound.Settings = string(raw)
	plan, err = buildGhostLinkPolicyPlan(inbound, snapshot, rows, traffic, now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	row = plan.rows["alice-device"]
	if row.PolicyState != ghostLinkPolicyManualOff || row.ManualEnabled || row.EffectiveEnabled {
		t.Fatalf("manual disable policy = %+v", row)
	}
}

func TestGhostLinkPolicyResetRebasesRuntimeWithoutClearingLifetimeCounters(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	inbound, snapshot, rows, traffic := ghostLinkPolicyFixture(t, now)
	rows["alice-device"] = &model.GhostLinkUsageWatermark{
		Id: 1, InboundId: 1, ClientId: "alice-device", Email: "alice@example.test",
		StoreId: snapshot.StoreID, UploadBytes: 100, DownloadBytes: 200,
		PolicyVersion: 1, ManualEnabled: true, EffectiveEnabled: true,
		PolicyState: ghostLinkPolicyOK, QuotaBytes: 1000, ExpiryTime: now.Add(time.Hour).UnixMilli(),
		MaxTrafficBytes: 500, TrafficReset: "daily", NextResetAt: now.Add(-time.Minute).UnixMilli(),
	}
	plan, err := buildGhostLinkPolicyPlan(inbound, snapshot, rows, traffic, now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	row := plan.rows["alice-device"]
	if !plan.resetIDs["alice-device"] || row.NextResetAt <= now.UnixMilli() {
		t.Fatalf("reset plan = %+v", plan)
	}
	if row.UploadBytes != 100 || row.DownloadBytes != 200 {
		t.Fatalf("reset changed lifetime Core counters: %+v", row)
	}
	if plan.policies["alice-device"].MaxTrafficBytes != 1300 {
		t.Fatalf("post-reset runtime ceiling = %d, want 1300", plan.policies["alice-device"].MaxTrafficBytes)
	}

	settings, err := ghostlink.ParseSettings(inbound.Settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.Clients[0].Enable = false
	raw, _ := json.Marshal(settings)
	inbound.Settings = string(raw)
	plan, err = buildGhostLinkPolicyPlan(inbound, snapshot, rows, traffic, now, time.UTC, map[string]bool{"alice-device": true})
	if err != nil {
		t.Fatal(err)
	}
	row = plan.rows["alice-device"]
	if row.EffectiveEnabled || row.ManualEnabled || row.PolicyState != ghostLinkPolicyManualOff {
		t.Fatalf("reset re-enabled manually disabled user: %+v", row)
	}
}

func TestGhostLinkPolicyPendingUsageFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	inbound, snapshot, rows, traffic := ghostLinkPolicyFixture(t, now)
	snapshot.Users[0].UploadBytes = 50
	rows["alice-device"] = &model.GhostLinkUsageWatermark{
		Id: 1, InboundId: 1, ClientId: "alice-device", Email: "alice@example.test",
		StoreId: snapshot.StoreID, UploadBytes: 100, DownloadBytes: 200,
		State: "pending", ErrorCode: "cumulative_counter_regressed", PolicyVersion: 1,
	}
	plan, err := buildGhostLinkPolicyPlan(inbound, snapshot, rows, traffic, now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.pending || plan.policies["alice-device"].Enabled ||
		plan.rows["alice-device"].PolicyState != ghostLinkPolicyPendingUsage {
		t.Fatalf("pending policy plan = %+v", plan)
	}
}

func TestGhostLinkResetBoundariesUsePanelTimezone(t *testing.T) {
	loc := time.FixedZone("test", 8*60*60)
	now := time.Date(2026, 11, 4, 12, 34, 0, 0, loc)
	if got := ghostLinkNextResetAt(now, "hourly", 0, loc); got != time.Date(2026, 11, 4, 13, 0, 0, 0, loc).UnixMilli() {
		t.Fatalf("hourly reset = %d", got)
	}
	if got := ghostLinkNextResetAt(now, "daily", 0, loc); got != time.Date(2026, 11, 5, 0, 0, 0, 0, loc).UnixMilli() {
		t.Fatalf("daily reset = %d", got)
	}
	if got := ghostLinkNextResetAt(now, "weekly", 0, loc); got != time.Date(2026, 11, 8, 0, 0, 0, 0, loc).UnixMilli() {
		t.Fatalf("weekly reset = %d", got)
	}
	if got := ghostLinkNextResetAt(now, "monthly", 31, loc); got != time.Date(2026, 11, 30, 0, 0, 0, 0, loc).UnixMilli() {
		t.Fatalf("monthly reset = %d", got)
	}
}

func TestGhostLinkPeriodicResetCommitsLedgerAndKeepsLifetimeCounter(t *testing.T) {
	now := time.Now().UTC()
	inbound, svc := seedGhostLinkUsageInbound(t)
	var settings ghostlink.Settings
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		t.Fatal(err)
	}
	settings.Clients[0].TrafficReset = "daily"
	settings.Clients[0].TotalGB = 1000
	raw, _ := json.Marshal(settings)
	inbound.Settings = string(raw)
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", inbound.Id).
		Update("settings", inbound.Settings).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := ghostlink.UsageSnapshot{
		StoreID: "store:periodic-reset",
		Source:  "test",
		Users: []ghostlink.UsageUser{{
			ClientID: "alice-device", UploadBytes: 100, DownloadBytes: 200,
		}},
	}
	if _, err := svc.recordGhostLinkUsageSnapshot(context.Background(), inbound, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(&xray.ClientTraffic{}).Where("email = ?", "alice@example.test").
		Updates(map[string]any{"up": 300, "down": 400}).Error; err != nil {
		t.Fatal(err)
	}
	var row model.GhostLinkUsageWatermark
	if err := database.GetDB().Where("client_id = ?", "alice-device").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	row.PolicyVersion = 1
	row.TrafficReset = "daily"
	row.NextResetAt = now.Add(-time.Minute).UnixMilli()
	row.MaxTrafficBytes = 1000
	row.EffectiveEnabled = true
	row.ManualEnabled = true
	row.PolicyState = ghostLinkPolicyOK
	if err := database.GetDB().Save(&row).Error; err != nil {
		t.Fatal(err)
	}
	plan, err := svc.loadGhostLinkPolicyPlan(context.Background(), inbound, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.resetIDs["alice-device"] {
		t.Fatalf("periodic reset was not due: %+v", plan)
	}
	if err := runSerializedTx(func(tx *gorm.DB) error {
		return commitGhostLinkPolicyPlanTx(tx, plan)
	}); err != nil {
		t.Fatal(err)
	}
	var traffic xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", "alice@example.test").First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if traffic.Up != 0 || traffic.Down != 0 {
		t.Fatalf("periodic reset did not zero panel ledger: %+v", traffic)
	}
	if err := database.GetDB().Where("client_id = ?", "alice-device").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.UploadBytes != 100 || row.DownloadBytes != 200 || row.NextResetAt <= now.UnixMilli() {
		t.Fatalf("periodic reset damaged lifetime watermark: %+v", row)
	}
}
