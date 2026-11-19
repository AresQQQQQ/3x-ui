package service

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/dbtest"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
)

const conflictTestUUID = "11111111-2222-4333-8444-555555555555"

func initConflictTestDB(t *testing.T) {
	t.Helper()
	dbtest.InitDB(t, filepath.Join(t.TempDir(), "x-ui.db"))
}

func seedConflictInbound(t *testing.T, tag string, protocol model.Protocol, port int) *model.Inbound {
	t.Helper()
	inbound := &model.Inbound{
		UserId:   1,
		Tag:      tag,
		Protocol: protocol,
		Enable:   true,
		Port:     port,
	}
	if err := database.GetDB().Create(inbound).Error; err != nil {
		t.Fatalf("seed inbound %s: %v", tag, err)
	}
	return inbound
}

func seedConflictClient(t *testing.T, email, clientID string, inboundIDs ...int) *model.ClientRecord {
	t.Helper()
	db := database.GetDB()
	record := &model.ClientRecord{
		Email:  email,
		UUID:   clientID,
		SubID:  "sub-" + strings.ReplaceAll(email, "@", "-"),
		Enable: true,
	}
	if err := db.Create(record).Error; err != nil {
		t.Fatalf("seed client: %v", err)
	}
	for _, inboundID := range inboundIDs {
		if err := db.Create(&model.ClientInbound{
			ClientId:  record.Id,
			InboundId: inboundID,
		}).Error; err != nil {
			t.Fatalf("seed client inbound: %v", err)
		}
	}
	return record
}

func TestValidateClientAttachmentCompatibility(t *testing.T) {
	initConflictTestDB(t)
	ghost := seedConflictInbound(t, "conflict-ghost", model.GhostLink, 24443)
	mtproto := seedConflictInbound(t, "conflict-mtproto", model.MTProto, 24444)
	vless := seedConflictInbound(t, "conflict-vless", model.VLESS, 24445)
	trojan := seedConflictInbound(t, "conflict-trojan", model.Trojan, 24446)

	tests := []struct {
		name     string
		ids      []int
		clientID string
		want     string
	}{
		{name: "ghostlink custom ID", ids: []int{ghost.Id}, clientID: "custom-client-01"},
		{name: "ghostlink plus trojan custom ID", ids: []int{ghost.Id, trojan.Id}, clientID: "custom-client-01"},
		{name: "ghostlink plus vless canonical UUID", ids: []int{ghost.Id, vless.Id}, clientID: conflictTestUUID},
		{name: "ghostlink plus vless compact UUID", ids: []int{ghost.Id, vless.Id}, clientID: strings.ReplaceAll(conflictTestUUID, "-", "")},
		{name: "ghostlink plus vless custom ID", ids: []int{ghost.Id, vless.Id}, clientID: "custom-client-01", want: "requires a standard"},
		{name: "ghostlink plus vless noncanonical 36 chars", ids: []int{ghost.Id, vless.Id}, clientID: strings.ReplaceAll(conflictTestUUID, "-", "") + "abcd", want: "requires a standard"},
		{name: "ghostlink plus mtproto", ids: []int{ghost.Id, mtproto.Id}, clientID: conflictTestUUID, want: "conflicting Secret formats"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClientAttachmentCompatibility(database.GetDB(), tc.ids, tc.clientID)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestClientMutationPathsRejectMixedCustomClientID(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		initConflictTestDB(t)
		ghost := seedConflictInbound(t, "create-ghost", model.GhostLink, 24501)
		vless := seedConflictInbound(t, "create-vless", model.VLESS, 24502)
		_, err := (&ClientService{}).Create(&InboundService{}, &ClientCreatePayload{
			Client: model.Client{
				Email:  "create@conflict",
				ID:     "custom-client-01",
				SubID:  "create-conflict-sub",
				Secret: "0123456789abcdef0123456789abcdef",
				Enable: true,
			},
			InboundIds: []int{ghost.Id, vless.Id},
		})
		if err == nil || !strings.Contains(err.Error(), "requires a standard") {
			t.Fatalf("Create error = %v", err)
		}
	})

	t.Run("update", func(t *testing.T) {
		initConflictTestDB(t)
		ghost := seedConflictInbound(t, "update-ghost", model.GhostLink, 24511)
		vless := seedConflictInbound(t, "update-vless", model.VLESS, 24512)
		record := seedConflictClient(t, "update@conflict", "custom-client-01", ghost.Id, vless.Id)
		_, err := (&ClientService{}).Update(
			&InboundService{},
			record.Id,
			model.Client{
				Email:  record.Email,
				ID:     "custom-client-01",
				SubID:  record.SubID,
				Enable: true,
			},
			0,
		)
		if err == nil || !strings.Contains(err.Error(), "requires a standard") {
			t.Fatalf("Update error = %v", err)
		}
	})

	t.Run("attach", func(t *testing.T) {
		initConflictTestDB(t)
		ghost := seedConflictInbound(t, "attach-ghost", model.GhostLink, 24521)
		vless := seedConflictInbound(t, "attach-vless", model.VLESS, 24522)
		record := seedConflictClient(t, "attach@conflict", "custom-client-01", ghost.Id)
		_, err := (&ClientService{}).Attach(&InboundService{}, record.Id, []int{vless.Id})
		if err == nil || !strings.Contains(err.Error(), "requires a standard") {
			t.Fatalf("Attach error = %v", err)
		}
	})

	t.Run("bulk attach", func(t *testing.T) {
		initConflictTestDB(t)
		ghost := seedConflictInbound(t, "bulk-ghost", model.GhostLink, 24531)
		vless := seedConflictInbound(t, "bulk-vless", model.VLESS, 24532)
		record := seedConflictClient(t, "bulk@conflict", "custom-client-01", ghost.Id)
		result, _, err := (&ClientService{}).BulkAttach(
			&InboundService{},
			[]string{record.Email},
			[]int{vless.Id},
		)
		if err != nil {
			t.Fatalf("BulkAttach returned hard error: %v", err)
		}
		if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "requires a standard") {
			t.Fatalf("BulkAttach result = %+v", result)
		}
	})

	t.Run("bulk create", func(t *testing.T) {
		initConflictTestDB(t)
		ghost := seedConflictInbound(t, "bulk-create-ghost", model.GhostLink, 24541)
		vless := seedConflictInbound(t, "bulk-create-vless", model.VLESS, 24542)
		result, _, err := (&ClientService{}).BulkCreate(
			&InboundService{},
			[]ClientCreatePayload{{
				Client: model.Client{
					Email:  "bulk-create@conflict",
					ID:     "custom-client-01",
					SubID:  "bulk-create-conflict-sub",
					Secret: "0123456789abcdef0123456789abcdef",
					Enable: true,
				},
				InboundIds: []int{ghost.Id, vless.Id},
			}},
		)
		if err != nil {
			t.Fatalf("BulkCreate returned hard error: %v", err)
		}
		if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0].Reason, "requires a standard") {
			t.Fatalf("BulkCreate result = %+v", result)
		}
	})
}

func TestInboundAttachmentCompatibilityBatch(t *testing.T) {
	initConflictTestDB(t)
	ghost := seedConflictInbound(t, "batch-ghost", model.GhostLink, 24551)
	vless := seedConflictInbound(t, "batch-vless", model.VLESS, 24552)
	record := seedConflictClient(t, "batch@conflict", "custom-client-01", ghost.Id)

	err := validateInboundAttachmentCompatibility(
		database.GetDB(),
		vless.Id,
		vless.Protocol,
		[]model.Client{{Email: record.Email, ID: record.UUID, Enable: true}},
	)
	if err == nil || !strings.Contains(err.Error(), "requires a standard") {
		t.Fatalf("custom mixed identity error = %v", err)
	}
	if err := validateInboundAttachmentCompatibility(
		database.GetDB(),
		vless.Id,
		vless.Protocol,
		[]model.Client{{Email: record.Email, ID: conflictTestUUID, Enable: true}},
	); err != nil {
		t.Fatalf("standard UUID should satisfy mixed identity: %v", err)
	}
}
