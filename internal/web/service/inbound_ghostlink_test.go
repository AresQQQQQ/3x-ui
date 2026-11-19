package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/web/runtime"
	"github.com/mhsanaei/3x-ui/v3/internal/xray"
	"gorm.io/gorm"
)

const ghostLinkTestSecret = "ghostlink-3xui-test-secret-at-least-32-bytes"

type ghostLinkControllerFake struct {
	mu            sync.Mutex
	reconciles    []*model.Inbound
	stops         int
	statuses      int
	reconcileErr  error
	stopErr       error
	lastStatus    ghostlink.Status
	statusErr     error
	reconcileFunc func(*model.Inbound)
	pending       *ghostlink.PendingUserOperation
	applyCalls    int
	confirmCalls  int
	rollbackCalls int
	resolveCalls  int
	applyErr      error
	confirmErr    error
	lastOld       *model.Inbound
	lastNew       *model.Inbound
	usageCalls    int
	usageErr      error
	usageFunc     func(*model.Inbound) ghostlink.UsageSnapshot
	lastPolicies  map[string]ghostlink.RuntimePolicy
}

func (f *ghostLinkControllerFake) Reconcile(_ context.Context, inbound *model.Inbound) (ghostlink.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copyInbound := *inbound
	f.reconciles = append(f.reconciles, &copyInbound)
	if f.reconcileFunc != nil {
		f.reconcileFunc(inbound)
	}
	if f.reconcileErr != nil {
		return ghostlink.Status{Reason: "start_failed"}, f.reconcileErr
	}
	f.lastStatus = ghostlink.Status{
		Expected:        inbound.Enable,
		Running:         true,
		Ready:           true,
		Listen:          inbound.Listen,
		DesiredRevision: "test-revision",
		ActiveRevision:  "test-revision",
		Reason:          "running",
	}
	return f.lastStatus, nil
}

func (f *ghostLinkControllerFake) ReconcileGhostLinkWithPolicies(
	ctx context.Context,
	inbound *model.Inbound,
	policies map[string]ghostlink.RuntimePolicy,
) (ghostlink.Status, error) {
	status, err := f.Reconcile(ctx, inbound)
	revision, revisionErr := ghostlink.UserRevisionForInboundWithPolicies(inbound, policies)
	if revisionErr != nil {
		return status, revisionErr
	}
	f.mu.Lock()
	f.lastPolicies = cloneRuntimePolicies(policies)
	f.lastStatus.DesiredUserRevision = revision
	f.lastStatus.RuntimeRevision = revision
	status = f.lastStatus
	f.mu.Unlock()
	return status, err
}

func (f *ghostLinkControllerFake) Stop(context.Context) (ghostlink.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stopErr != nil {
		return ghostlink.Status{Reason: "stop_failed"}, f.stopErr
	}
	f.lastStatus = ghostlink.Status{Reason: "stopped"}
	return f.lastStatus, nil
}

func (f *ghostLinkControllerFake) Status(context.Context, *model.Inbound) (ghostlink.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses++
	return f.lastStatus, f.statusErr
}

func (f *ghostLinkControllerFake) GhostLinkStatusWithPolicies(
	ctx context.Context,
	inbound *model.Inbound,
	policies map[string]ghostlink.RuntimePolicy,
) (ghostlink.Status, error) {
	status, err := f.Status(ctx, inbound)
	revision, revisionErr := ghostlink.UserRevisionForInboundWithPolicies(inbound, policies)
	if revisionErr != nil {
		return status, revisionErr
	}
	status.DesiredUserRevision = revision
	status.Expected = inbound != nil && inbound.Enable
	if inbound != nil {
		status.Listen = net.JoinHostPort(inbound.Listen, strconv.Itoa(inbound.Port))
	}
	if status.Running {
		status.RuntimeRevision = revision
		status.Ready = true
	} else {
		status.Ready = false
	}
	return status, err
}

func (f *ghostLinkControllerFake) GhostLinkUsageSnapshot(_ context.Context, inbound *model.Inbound) (ghostlink.UsageSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usageCalls++
	if f.usageErr != nil {
		return ghostlink.UsageSnapshot{}, f.usageErr
	}
	if f.usageFunc != nil {
		return f.usageFunc(inbound), nil
	}
	users := []ghostlink.UsageUser{}
	if identities, identityErr := ghostlink.ClientIdentityMap(inbound); identityErr == nil {
		for clientID := range identities {
			users = append(users, ghostlink.UsageUser{ClientID: clientID})
		}
	}
	return ghostlink.UsageSnapshot{
		StoreID:             "store:test",
		InstanceID:          "instance:test",
		GeneratedAtUnixNano: time.Now().UnixNano(),
		Source:              "uds",
		Users:               users,
	}, nil
}

func (f *ghostLinkControllerFake) ApplyUserConfig(
	ctx context.Context,
	oldInbound *model.Inbound,
	newInbound *model.Inbound,
	expected string,
	requestID string,
) (ghostlink.UserOperation, error) {
	return f.applyUserConfigWithPolicies(ctx, oldInbound, newInbound, nil, nil, expected, requestID)
}

func (f *ghostLinkControllerFake) ApplyUserPolicyConfig(
	ctx context.Context,
	oldInbound *model.Inbound,
	newInbound *model.Inbound,
	oldPolicies map[string]ghostlink.RuntimePolicy,
	newPolicies map[string]ghostlink.RuntimePolicy,
	expected string,
	requestID string,
) (ghostlink.UserOperation, error) {
	return f.applyUserConfigWithPolicies(ctx, oldInbound, newInbound, oldPolicies, newPolicies, expected, requestID)
}

func (f *ghostLinkControllerFake) applyUserConfigWithPolicies(
	_ context.Context,
	oldInbound *model.Inbound,
	newInbound *model.Inbound,
	oldPolicies map[string]ghostlink.RuntimePolicy,
	newPolicies map[string]ghostlink.RuntimePolicy,
	_ string,
	requestID string,
) (ghostlink.UserOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyCalls++
	f.lastPolicies = cloneRuntimePolicies(newPolicies)
	oldCopy := *oldInbound
	newCopy := *newInbound
	f.lastOld = &oldCopy
	f.lastNew = &newCopy
	if f.applyErr != nil {
		return ghostlink.UserOperation{}, f.applyErr
	}
	oldRevision, _ := ghostlink.UserRevisionForInboundWithPolicies(oldInbound, oldPolicies)
	newRevision, _ := ghostlink.UserRevisionForInboundWithPolicies(newInbound, newPolicies)
	f.pending = &ghostlink.PendingUserOperation{
		OperationID: requestID, RequestID: requestID, Phase: "applied",
		OldUserRevision: oldRevision, NewUserRevision: newRevision,
	}
	f.lastStatus = ghostlink.Status{
		Expected: true, Running: true, Ready: true, RuntimeReady: true,
		DesiredUserRevision: newRevision, RuntimeRevision: newRevision,
		Reason: "running",
	}
	return ghostlink.UserOperation{
		OperationID: requestID, RequestID: requestID,
		OldUserRevision: oldRevision, NewUserRevision: newRevision,
		Hot: true, Status: f.lastStatus,
	}, nil
}

func cloneRuntimePolicies(policies map[string]ghostlink.RuntimePolicy) map[string]ghostlink.RuntimePolicy {
	if policies == nil {
		return nil
	}
	out := make(map[string]ghostlink.RuntimePolicy, len(policies))
	for id, policy := range policies {
		out[id] = policy
	}
	return out
}

func (f *ghostLinkControllerFake) ConfirmUserConfig(_ context.Context, operationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmCalls++
	if f.confirmErr != nil {
		return f.confirmErr
	}
	if f.pending != nil && f.pending.OperationID == operationID {
		f.pending = nil
	}
	return nil
}

func (f *ghostLinkControllerFake) RollbackUserConfig(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rollbackCalls++
	f.pending = nil
	return nil
}

func (f *ghostLinkControllerFake) ResolvePendingUserConfig(_ context.Context, desiredUserRevision string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveCalls++
	if f.pending == nil {
		return "none", nil
	}
	action := "rolled_back"
	if desiredUserRevision == f.pending.NewUserRevision {
		action = "rolled_forward"
	}
	f.pending = nil
	return action, nil
}

func (f *ghostLinkControllerFake) PendingUserConfig(context.Context) (ghostlink.PendingUserOperation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil {
		return ghostlink.PendingUserOperation{}, false, nil
	}
	return *f.pending, true, nil
}

func installGhostLinkController(t *testing.T, controller ghostlink.Controller) {
	t.Helper()
	previous := runtime.GetManager()
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{
		APIPort:   func() int { return 0 },
		GhostLink: controller,
	}))
	t.Cleanup(func() { runtime.SetManager(previous) })
}

func ghostLinkTestCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "ghostlink.example"},
		DNSNames:     []string{"ghostlink.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func ghostLinkSettingsJSON(t *testing.T) string {
	t.Helper()
	certPath, keyPath := ghostLinkTestCertificate(t)
	raw, err := json.Marshal(map[string]any{
		"version":         ghostlink.SchemaVersion,
		"certificateFile": certPath,
		"privateKeyFile":  keyPath,
		"serverName":      "ghostlink.example",
		"webBackendUrl":   "http://127.0.0.1:8080",
		"clients": []map[string]any{{
			"email":  "alice@example.test",
			"id":     "alice-device",
			"secret": ghostLinkTestSecret,
			"enable": true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestGhostLinkInboundFoundation(t *testing.T) {
	setupConflictDB(t)
	svc := &InboundService{}
	inbound := &model.Inbound{
		UserId:   1,
		Remark:   "GhostLink",
		Enable:   false,
		Listen:   "127.0.0.1",
		Port:     38443,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	}
	created, needRestart, err := svc.AddInbound(inbound)
	if err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	if needRestart {
		t.Fatal("GhostLink save must not request an Xray restart")
	}
	if created.Id == 0 || created.Protocol != model.GhostLink {
		t.Fatalf("created inbound = %#v", created)
	}
	if created.StreamSettings != "" {
		t.Fatalf("GhostLink streamSettings must stay empty, got %q", created.StreamSettings)
	}

	state, found, err := svc.GhostLinkDesiredState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("GhostLinkDesiredState: %v", err)
	}
	if !found || state.ShouldRun || state.Reason != "inbound_disabled" {
		t.Fatalf("desired state = found:%v %#v", found, state)
	}
	if state.InboundID != created.Id || state.Config.Listen != "127.0.0.1:38443" {
		t.Fatalf("desired state did not map the inbound: %#v", state)
	}

	second := &model.Inbound{
		UserId:   1,
		Remark:   "second GhostLink",
		Enable:   false,
		Listen:   "127.0.0.1",
		Port:     38444,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	}
	if _, _, err := svc.AddInbound(second); err == nil ||
		!strings.Contains(err.Error(), "exactly one GhostLink inbound") {
		t.Fatalf("second GhostLink inbound error = %v", err)
	}
}

func TestGhostLinkAddInboundInitializesPolicyBeforeStart(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	installGhostLinkController(t, controller)
	var settings ghostlink.Settings
	if err := json.Unmarshal([]byte(ghostLinkSettingsJSON(t)), &settings); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).UnixMilli()
	settings.Clients[0].TotalGB = 4096
	settings.Clients[0].ExpiryTime = expiry
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	inbound, _, err := (&InboundService{}).AddInbound(&model.Inbound{
		UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 38444,
		Protocol: model.GhostLink, Settings: string(raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	policy, ok := controller.lastPolicies["alice-device"]
	controller.mu.Unlock()
	if !ok || !policy.Enabled || policy.MaxTrafficBytes != 4096 ||
		policy.ExpiresAtUnixMilli != expiry {
		t.Fatalf("initial runtime policy = %+v", policy)
	}
	var watermark model.GhostLinkUsageWatermark
	if err := database.GetDB().Where("inbound_id = ? AND client_id = ?", inbound.Id, "alice-device").
		First(&watermark).Error; err != nil {
		t.Fatal(err)
	}
	if watermark.PolicyVersion != 1 || watermark.MaxTrafficBytes != 4096 ||
		watermark.ExpiryTime != expiry {
		t.Fatalf("initial policy watermark = %+v", watermark)
	}
}

func TestGhostLinkEnabledInboundLifecycleAndCompensation(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	inbound := &model.Inbound{
		UserId:   1,
		Remark:   "GhostLink lifecycle",
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38443,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	}
	created, needRestart, err := svc.AddInbound(inbound)
	if err != nil {
		t.Fatalf("enabled AddInbound: %v", err)
	}
	if needRestart {
		t.Fatal("GhostLink lifecycle requested an Xray restart")
	}
	if !created.Enable {
		t.Fatal("successful lifecycle start did not persist enable=true")
	}
	if len(controller.reconciles) != 1 || !controller.reconciles[0].Enable {
		t.Fatalf("controller reconciles = %#v", controller.reconciles)
	}

	if _, err := svc.SetInboundEnable(created.Id, true); err != nil {
		t.Fatalf("idempotent enable: %v", err)
	}
	if len(controller.reconciles) != 1 {
		t.Fatalf("idempotent enable reconciles = %d, want no redundant reconcile", len(controller.reconciles))
	}

	if _, _, err := svc.UpdateInbound(created); err == nil || !strings.Contains(err.Error(), "disable it before changing") {
		t.Fatalf("running inbound update error = %v", err)
	}
	if _, _, err := (&ClientService{}).BulkSetEnable(svc, []string{"alice@example.test"}, false); err == nil ||
		!strings.Contains(err.Error(), "disable it before changing") {
		t.Fatalf("running bulk enable error = %v", err)
	}
	if _, _, err := (&ClientService{}).BulkAdjust(svc, []string{"alice@example.test"}, 1, 0, "", nil, ""); err == nil ||
		!strings.Contains(err.Error(), "disable it before changing") {
		t.Fatalf("running bulk adjust error = %v", err)
	}

	clientSvc := &ClientService{}
	addedClient := model.Client{
		Email: "blocked@example.test", ID: "blocked-client",
		Secret: ghostLinkTestSecret, Enable: true,
	}
	if _, err := clientSvc.CreateOne(svc, created.Id, addedClient); err != nil {
		t.Fatalf("single client create: %v", err)
	}
	addedClient.Secret = "ghostlink-rotated-blocked-secret-00032"
	if _, err := clientSvc.UpdateByEmail(svc, addedClient.Email, addedClient, 0); err != nil {
		t.Fatalf("single client secret update: %v", err)
	}
	if changed, needRestart, err := clientSvc.SetClientEnableByEmail(svc, addedClient.Email, false); err != nil ||
		!changed || needRestart {
		t.Fatalf("single client disable changed=%v restart=%v err=%v", changed, needRestart, err)
	}
	if needRestart, err := clientSvc.DeleteByEmail(svc, "alice@example.test", true); err != nil || needRestart {
		t.Fatalf("single client delete needRestart=%v err=%v", needRestart, err)
	}
	if controller.applyCalls != 4 || controller.confirmCalls != 4 || controller.pending != nil {
		t.Fatalf("transaction calls apply=%d confirm=%d pending=%#v", controller.applyCalls, controller.confirmCalls, controller.pending)
	}
	if _, err := svc.GhostLinkLifecycleStatus(context.Background()); err != nil {
		t.Fatalf("GhostLinkLifecycleStatus: %v", err)
	}

	if _, err := svc.SetInboundEnable(created.Id, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if controller.stops != 1 {
		t.Fatalf("stop calls = %d, want 1", controller.stops)
	}
	stored, err := svc.GetInbound(created.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Enable {
		t.Fatal("disable persisted enable=true")
	}
}

func TestGhostLinkReconcileResolvesPendingOperationFromDBRevision(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	created, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38443,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	policies, err := currentGhostLinkRuntimePolicies(created)
	if err != nil {
		t.Fatal(err)
	}
	currentRevision, err := ghostlink.UserRevisionForInboundWithPolicies(created, policies)
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.pending = &ghostlink.PendingUserOperation{
		OperationID: "pending-db-new", RequestID: "pending-db-new", Phase: "applied",
		OldUserRevision: "sha256:old", NewUserRevision: currentRevision,
		NewShouldRun: true,
	}
	controller.mu.Unlock()
	if _, err := svc.ReconcileGhostLink(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.resolveCalls != 1 || controller.pending != nil {
		t.Fatalf("pending resolution calls=%d pending=%#v", controller.resolveCalls, controller.pending)
	}
}

func TestGhostLinkDeleteStopsBeforeRemovingInbound(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	created, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38443,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	needRestart, err := svc.DelInbound(created.Id)
	if err != nil {
		t.Fatalf("DelInbound: %v", err)
	}
	if needRestart {
		t.Fatal("GhostLink delete requested an Xray restart")
	}
	if controller.stops != 1 {
		t.Fatalf("stop calls = %d, want 1", controller.stops)
	}
	if _, err := svc.GetInbound(created.Id); err == nil {
		t.Fatal("GhostLink inbound remained after delete")
	}
}

func TestGhostLinkDeleteStopFailureKeepsInbound(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	created, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38443,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.stopErr = errors.New("systemd stop failed")
	controller.mu.Unlock()
	if _, err := svc.DelInbound(created.Id); err == nil || !strings.Contains(err.Error(), "systemd stop failed") {
		t.Fatalf("delete stop failure = %v", err)
	}
	stored, err := svc.GetInbound(created.Id)
	if err != nil {
		t.Fatalf("inbound was removed despite stop failure: %v", err)
	}
	if !stored.Enable {
		t.Fatal("stop failure changed desired enable state")
	}
}

func TestGhostLinkSingleClientDBCommitFailureRollsBackRuntime(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	created, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38443,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	const callback = "round4:fail-ghostlink-client-commit"
	if err := db.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "inbounds" {
			tx.AddError(errors.New("injected GhostLink client commit failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Update().Remove(callback) })

	added := model.Client{
		Email: "commit-failure@example.test", ID: "commit-failure",
		Secret: ghostLinkTestSecret, Enable: true,
	}
	if _, err := (&ClientService{}).CreateOne(svc, created.Id, added); err == nil ||
		!strings.Contains(err.Error(), "injected GhostLink client commit failure") {
		t.Fatalf("DB commit failure = %v", err)
	}
	if controller.rollbackCalls != 1 || controller.pending != nil {
		t.Fatalf("rollback calls=%d pending=%#v", controller.rollbackCalls, controller.pending)
	}
	stored, err := svc.GetInbound(created.Id)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := svc.GetClients(stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 || clients[0].Email != "alice@example.test" {
		t.Fatalf("failed DB commit changed clients: %#v", clients)
	}
}

func TestGhostLinkSingleClientConfirmationFailureStaysPending(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{confirmErr: errors.New("injected confirmation failure")}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	created, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38443,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	added := model.Client{
		Email: "confirm-failure@example.test", ID: "confirm-failure",
		Secret: ghostLinkTestSecret, Enable: true,
	}
	if _, err := (&ClientService{}).CreateOne(svc, created.Id, added); err == nil ||
		!strings.Contains(err.Error(), "confirmation is pending recovery") {
		t.Fatalf("confirmation failure = %v", err)
	}
	if controller.pending == nil || controller.rollbackCalls != 0 {
		t.Fatalf("pending=%#v rollbackCalls=%d", controller.pending, controller.rollbackCalls)
	}
	stored, err := svc.GetInbound(created.Id)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := svc.GetClients(stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 2 {
		t.Fatalf("DB commit did not persist before confirmation failure: %#v", clients)
	}

	controller.mu.Lock()
	controller.confirmErr = nil
	controller.mu.Unlock()
	if _, err := svc.ReconcileGhostLink(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.pending != nil || controller.resolveCalls == 0 {
		t.Fatalf("pending recovery calls=%d pending=%#v", controller.resolveCalls, controller.pending)
	}
}

func TestGhostLinkLastClientDeleteCapturesFinalUsageTail(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	controller.usageFunc = func(inbound *model.Inbound) ghostlink.UsageSnapshot {
		settings, err := ghostlink.ClientIdentityMap(inbound)
		if err != nil {
			t.Fatal(err)
		}
		upload := uint64(180)
		if len(settings) > 0 {
			upload = 100
		}
		return ghostlink.UsageSnapshot{
			StoreID: "store:tail-test",
			Users: []ghostlink.UsageUser{{
				ClientID:    "alice-device",
				UploadBytes: upload,
			}},
			Source: "test",
		}
	}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	created, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38446,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&ClientService{}).DeleteByEmail(svc, "alice@example.test", true); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.GetInbound(created.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Up != 80 || stored.Down != 0 {
		t.Fatalf("final usage tail was not charged: up=%d down=%d", stored.Up, stored.Down)
	}
	if controller.usageCalls < 2 {
		t.Fatalf("usage snapshots before/after delete = %d, want at least 2", controller.usageCalls)
	}
}

func TestGhostLinkNewClientBaselineDoesNotDropImmediateTraffic(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	controller.usageFunc = func(*model.Inbound) ghostlink.UsageSnapshot {
		call := controller.usageCalls
		users := []ghostlink.UsageUser{{
			ClientID:    "alice-device",
			UploadBytes: 100,
		}}
		if call >= 3 {
			charlieUpload := uint64(5)
			if call >= 4 {
				charlieUpload = 9
			}
			users = append(users, ghostlink.UsageUser{
				ClientID:    "charlie-device",
				UploadBytes: charlieUpload,
			})
		}
		return ghostlink.UsageSnapshot{
			StoreID: "store:new-client-test",
			Users:   users,
			Source:  "test",
		}
	}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	created, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38447,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	charlie := model.Client{
		Email: "charlie-new@example.test", ID: "charlie-device",
		Secret: ghostLinkTestSecret, Enable: true,
	}
	if _, err := (&ClientService{}).CreateOne(svc, created.Id, charlie); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CollectGhostLinkUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	var traffic xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", charlie.Email).First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if traffic.Up != 9 || traffic.Down != 0 {
		t.Fatalf("new-client immediate traffic was not fully accounted: %+v", traffic)
	}
}

func TestGhostLinkEmailChangeKeepsUsageSequence(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	controller.usageFunc = func(*model.Inbound) ghostlink.UsageSnapshot {
		upload := uint64(100)
		switch controller.usageCalls {
		case 3:
			upload = 150
		case 4:
			upload = 180
		}
		return ghostlink.UsageSnapshot{
			StoreID: "store:email-change",
			Users: []ghostlink.UsageUser{{
				ClientID:    "alice-device",
				UploadBytes: upload,
			}},
			Source: "test",
		}
	}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	inbound, _, err := svc.AddInbound(&model.Inbound{
		UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 38448,
		Protocol: model.GhostLink, Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	updated := model.Client{
		Email: "alice-renamed@example.test", ID: "alice-device",
		Secret: ghostLinkTestSecret, Enable: true,
	}
	if _, err := (&ClientService{}).UpdateByEmail(
		svc, "alice@example.test", updated, 0,
	); err != nil {
		t.Fatal(err)
	}
	result, err := svc.CollectGhostLinkUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadDelta != 30 {
		t.Fatalf("post-email-change delta = %d, want 30", result.UploadDelta)
	}
	var watermark model.GhostLinkUsageWatermark
	if err := database.GetDB().Where("inbound_id = ? AND client_id = ?", inbound.Id, "alice-device").
		First(&watermark).Error; err != nil {
		t.Fatal(err)
	}
	if watermark.Email != updated.Email || watermark.UploadBytes != 180 {
		t.Fatalf("email-change watermark = %+v", watermark)
	}
}

func TestGhostLinkClientIDRotationCapturesOldTailAndBaselinesNewID(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{}
	controller.usageFunc = func(*model.Inbound) ghostlink.UsageSnapshot {
		users := []ghostlink.UsageUser{{
			ClientID:    "alice-device",
			UploadBytes: 100,
		}}
		switch controller.usageCalls {
		case 3:
			users[0].UploadBytes = 150
			users = append(users, ghostlink.UsageUser{ClientID: "alice-rotated", UploadBytes: 5})
		case 4:
			users[0].UploadBytes = 150
			users = append(users, ghostlink.UsageUser{ClientID: "alice-rotated", UploadBytes: 20})
		}
		return ghostlink.UsageSnapshot{
			StoreID: "store:id-rotation",
			Users:   users,
			Source:  "test",
		}
	}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	inbound, _, err := svc.AddInbound(&model.Inbound{
		UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 38449,
		Protocol: model.GhostLink, Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	rotated := model.Client{
		Email: "alice@example.test", ID: "alice-rotated",
		Secret: ghostLinkTestSecret, Enable: true,
	}
	if _, err := (&ClientService{}).UpdateByEmail(
		svc, "alice@example.test", rotated, 0,
	); err != nil {
		t.Fatal(err)
	}
	result, err := svc.CollectGhostLinkUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadDelta != 20 {
		t.Fatalf("post-rotation delta = %d, want new ID's 20 bytes", result.UploadDelta)
	}
	var newWatermark model.GhostLinkUsageWatermark
	if err := database.GetDB().Where("inbound_id = ? AND client_id = ?", inbound.Id, "alice-rotated").
		First(&newWatermark).Error; err != nil {
		t.Fatal(err)
	}
	if newWatermark.UploadBytes != 20 || newWatermark.Email != "alice@example.test" {
		t.Fatalf("rotated watermark = %+v", newWatermark)
	}
}

func TestGhostLinkEnabledStartFailureIsCompensated(t *testing.T) {
	setupConflictDB(t)
	controller := &ghostLinkControllerFake{reconcileErr: errors.New("systemd start failed")}
	installGhostLinkController(t, controller)
	svc := &InboundService{}
	inbound, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38443,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err == nil || !strings.Contains(err.Error(), "saved disabled") {
		t.Fatalf("start failure error = %v", err)
	}
	if inbound == nil || inbound.Id == 0 {
		t.Fatalf("failed start did not return the compensated inbound: %#v", inbound)
	}
	stored, getErr := svc.GetInbound(inbound.Id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.Enable {
		t.Fatal("failed GhostLink start remained enabled in the database")
	}
}

func TestGhostLinkProtocolBoundary(t *testing.T) {
	cases := []struct {
		protocol model.Protocol
		outside  bool
	}{
		{model.GhostLink, true},
		{model.MTProto, true},
		{model.TUIC, true},
		{model.AmneziaWG, true},
		{model.VLESS, false},
		{model.MASQUE, false},
	}
	for _, tc := range cases {
		if got := servedOutsideXray(tc.protocol); got != tc.outside {
			t.Errorf("servedOutsideXray(%s) = %v, want %v", tc.protocol, got, tc.outside)
		}
	}
	if got := inboundTransports(model.GhostLink, "", ""); got != transportTCP|transportUDP {
		t.Fatalf("GhostLink transports = %#b, want TCP+UDP", got)
	}
}

func TestGhostLinkPortConflictCoversBothTCPAndUDP(t *testing.T) {
	cases := []struct {
		name           string
		protocol       model.Protocol
		streamSettings string
		port           int
		want           transportBits
	}{
		{name: "TCP", protocol: model.VLESS, streamSettings: `{"network":"tcp"}`, port: 38443, want: transportTCP},
		{name: "UDP", protocol: model.Hysteria, port: 38444, want: transportUDP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupConflictDB(t)
			seedInboundConflict(t, "existing-"+tc.name, "127.0.0.1", tc.port, tc.protocol, tc.streamSettings, `{}`)
			probe := &model.Inbound{Protocol: model.GhostLink, Listen: "127.0.0.1", Port: tc.port}
			conflict, err := (&InboundService{}).checkPortConflict(probe, 0)
			if err != nil {
				t.Fatal(err)
			}
			if conflict == nil || conflict.Transports&tc.want == 0 {
				t.Fatalf("GhostLink did not detect the %s port conflict: %#v", tc.name, conflict)
			}
		})
	}
}

func TestGhostLinkClientCRUDUpdatesDesiredStateWithoutXrayRestart(t *testing.T) {
	setupConflictDB(t)
	svc := &InboundService{}
	created, _, err := svc.AddInbound(&model.Inbound{
		UserId:   1,
		Remark:   "GhostLink clients",
		Enable:   false,
		Listen:   "127.0.0.1",
		Port:     38445,
		Protocol: model.GhostLink,
		Settings: ghostLinkSettingsJSON(t),
	})
	if err != nil {
		t.Fatalf("AddInbound: %v", err)
	}

	var settings ghostlink.Settings
	if err := json.Unmarshal([]byte(created.Settings), &settings); err != nil {
		t.Fatal(err)
	}
	settings.Clients = append(settings.Clients, model.Client{
		Email: "short-secret@example.test", ID: "short-secret", Secret: "short", Enable: true,
	})
	wire, err := json.Marshal(settings.Clients[len(settings.Clients)-1:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&ClientService{}).AddInboundClient(svc, &model.Inbound{
		Id:       created.Id,
		Settings: `{"clients":` + string(wire) + `}`,
	}); err == nil || !strings.Contains(err.Error(), "32-1024 bytes") {
		t.Fatalf("invalid GhostLink client error = %v", err)
	}

	settings.Clients[len(settings.Clients)-1].Secret = ghostLinkTestSecret
	wire, err = json.Marshal(settings.Clients[len(settings.Clients)-1:])
	if err != nil {
		t.Fatal(err)
	}
	needRestart, err := (&ClientService{}).AddInboundClient(svc, &model.Inbound{
		Id:       created.Id,
		Settings: `{"clients":` + string(wire) + `}`,
	})
	if err != nil {
		t.Fatalf("AddInboundClient: %v", err)
	}
	if needRestart {
		t.Fatal("GhostLink client save must not request an Xray restart")
	}

	state, found, err := svc.GhostLinkDesiredState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !found || state.UserCount != 2 || state.Reason != "inbound_disabled" {
		t.Fatalf("desired state after client CRUD = found:%v %#v", found, state)
	}
	if _, ok := state.Config.Users["short-secret"]; !ok {
		t.Fatalf("new GhostLink client was not mapped into desired config: %#v", state.Config.Users)
	}
}
