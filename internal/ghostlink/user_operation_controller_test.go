package ghostlink

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
)

type transactionFakeControl struct {
	mu               sync.Mutex
	manager          *fakeServiceManager
	users            map[string]RuntimeUser
	revision         string
	applyCalls       int
	loseNextResponse bool
	failBeforeApply  error
	listen           string
	peerUID          uint32
}

func (c *transactionFakeControl) Status(context.Context) (controlResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manager.mu.Lock()
	snapshot := c.manager.snapshot
	c.manager.mu.Unlock()
	return controlResponse{
		OK:           true,
		PID:          snapshot.PID,
		PeerPID:      snapshot.PID,
		PeerUID:      c.peerUID,
		InstanceID:   "transaction-test-instance",
		Serving:      snapshot.Active,
		H2Listen:     c.listen,
		H3Listen:     c.listen,
		ActiveUsers:  enabledRuntimeUsers(c.users),
		UserRevision: c.revision,
	}, nil
}

func (c *transactionFakeControl) Usage(context.Context) (controlResponse, error) {
	return controlResponse{
		OK:         true,
		InstanceID: "transaction-test-instance",
		Usage: &RuntimeUsageSnapshot{
			StoreID:    "store:transaction-test",
			InstanceID: "transaction-test-instance",
		},
	}, nil
}

func (c *transactionFakeControl) ApplyUsers(
	_ context.Context,
	requestID string,
	expectedRevision string,
	users map[string]RuntimeUser,
) (controlResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failBeforeApply != nil {
		return controlResponse{}, c.failBeforeApply
	}
	if expectedRevision != c.revision {
		return controlResponse{}, errors.New("stale revision")
	}
	target, err := RuntimeUsersRevision(users)
	if err != nil {
		return controlResponse{}, err
	}
	c.users = cloneRuntimeUsers(users)
	c.revision = target
	c.applyCalls++
	if c.loseNextResponse {
		c.loseNextResponse = false
		return controlResponse{}, errors.New("simulated lost apply response")
	}
	status, _ := c.statusUnlocked()
	status.OK = true
	return status, nil
}

func (c *transactionFakeControl) statusUnlocked() (controlResponse, error) {
	c.manager.mu.Lock()
	snapshot := c.manager.snapshot
	c.manager.mu.Unlock()
	return controlResponse{
		OK:           true,
		PID:          snapshot.PID,
		PeerPID:      snapshot.PID,
		PeerUID:      c.peerUID,
		InstanceID:   "transaction-test-instance",
		Serving:      snapshot.Active,
		H2Listen:     c.listen,
		H3Listen:     c.listen,
		ActiveUsers:  enabledRuntimeUsers(c.users),
		UserRevision: c.revision,
	}, nil
}

func cloneRuntimeUsers(users map[string]RuntimeUser) map[string]RuntimeUser {
	clone := make(map[string]RuntimeUser, len(users))
	for id, user := range users {
		clone[id] = user
	}
	return clone
}

func enabledRuntimeUsers(users map[string]RuntimeUser) int {
	count := 0
	for _, user := range users {
		if user.Enabled {
			count++
		}
	}
	return count
}

func installTransactionControl(t *testing.T, f lifecycleFixture) (*systemdController, *transactionFakeControl) {
	t.Helper()
	control := &transactionFakeControl{
		manager: f.manager,
		listen:  "127.0.0.1:38443",
		peerUID: f.cfg.controlServerUID,
		users: map[string]RuntimeUser{"alice-device": {
			Secret:             testSecret,
			Enabled:            true,
			PolicyVersion:      1,
			ExpiresAtUnixMilli: 0, MaxConnections: -1, MaxTCPStreams: -1, MaxUDPRoutes: -1,
		}},
		revision: mustRuntimeUserRevision(t, map[string]RuntimeUser{"alice-device": {
			Secret: testSecret, Enabled: true, PolicyVersion: 1,
			MaxConnections: -1, MaxTCPStreams: -1, MaxUDPRoutes: -1,
		}}),
	}
	f.cfg.controlClient = control
	controller := newSystemdController(f.cfg, f.manager)
	if _, err := controller.Reconcile(context.Background(), f.inbound); err != nil {
		t.Fatalf("start transactional fixture: %v", err)
	}
	return controller, control
}

func mustRuntimeUserRevision(t *testing.T, users map[string]RuntimeUser) string {
	t.Helper()
	revision, err := RuntimeUsersRevision(users)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func mutateGhostLinkClient(t *testing.T, inbound *model.Inbound, mutate func(*model.Client)) *model.Inbound {
	t.Helper()
	var settings Settings
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		t.Fatal(err)
	}
	mutate(&settings.Clients[0])
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	next := *inbound
	next.Settings = string(raw)
	return &next
}

func TestUserOperationHotUpdateKeepsPIDAndPersistsBootConfig(t *testing.T) {
	f := newLifecycleFixture(t)
	controller, control := installTransactionControl(t, f)
	next := mutateGhostLinkClient(t, f.inbound, func(client *model.Client) {
		client.Secret = "ghostlink-rotated-secret-for-test-32"
	})
	initialRevision, err := UserRevisionForInbound(f.inbound)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := controller.ApplyUserConfig(
		context.Background(), f.inbound, next, initialRevision, "hot-operation-1",
	)
	if err != nil {
		t.Fatalf("ApplyUserConfig: %v", err)
	}
	if !operation.Hot || operation.PIDBefore != operation.PIDAfter || operation.PIDAfter == 0 {
		t.Fatalf("hot operation = %#v", operation)
	}
	if operation.NewUserRevision != control.revision {
		t.Fatalf("runtime revision = %s, want %s", control.revision, operation.NewUserRevision)
	}
	if _, exists, err := controller.PendingUserConfig(context.Background()); err != nil || !exists {
		t.Fatalf("pending journal exists=%v err=%v", exists, err)
	}
	config, err := osReadFile(f.cfg.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "ghostlink-rotated-secret-for-test-32") {
		t.Fatal("boot config did not receive the rotated secret")
	}
	if err := controller.ConfirmUserConfig(context.Background(), operation.OperationID); err != nil {
		t.Fatalf("ConfirmUserConfig: %v", err)
	}
	if _, exists, _ := controller.PendingUserConfig(context.Background()); exists {
		t.Fatal("operation journal remained after confirmation")
	}
}

func TestUserOperationRecoversLostApplyResponse(t *testing.T) {
	f := newLifecycleFixture(t)
	controller, control := installTransactionControl(t, f)
	control.mu.Lock()
	control.loseNextResponse = true
	control.mu.Unlock()
	next := mutateGhostLinkClient(t, f.inbound, func(client *model.Client) {
		client.Secret = "ghostlink-response-lost-secret-00032"
	})
	initialRevision, _ := UserRevisionForInbound(f.inbound)
	operation, err := controller.ApplyUserConfig(
		context.Background(), f.inbound, next, initialRevision, "lost-response-1",
	)
	if err != nil {
		t.Fatalf("lost response was not recovered: %v", err)
	}
	if operation.Status.RuntimeRevision != operation.NewUserRevision {
		t.Fatalf("status = %#v", operation.Status)
	}
	if err := controller.ConfirmUserConfig(context.Background(), operation.OperationID); err != nil {
		t.Fatal(err)
	}
}

func TestUserOperationRollbackRestoresRuntimeAndBootConfig(t *testing.T) {
	f := newLifecycleFixture(t)
	controller, control := installTransactionControl(t, f)
	next := mutateGhostLinkClient(t, f.inbound, func(client *model.Client) {
		client.Enable = false
	})
	initialRevision, _ := UserRevisionForInbound(f.inbound)
	operation, err := controller.ApplyUserConfig(
		context.Background(), f.inbound, next, initialRevision, "rollback-operation-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.RollbackUserConfig(context.Background(), operation.OperationID); err != nil {
		t.Fatal(err)
	}
	if control.revision != initialRevision {
		t.Fatalf("rollback revision = %s, initial %s", control.revision, initialRevision)
	}
	status, err := controller.Status(context.Background(), f.inbound)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || status.RuntimeRevision != initialRevision {
		t.Fatalf("rollback status = %#v", status)
	}
}

func TestUserOperationStopsWhenLastEnabledUserRemoved(t *testing.T) {
	f := newLifecycleFixture(t)
	controller, _ := installTransactionControl(t, f)
	next := mutateGhostLinkClient(t, f.inbound, func(client *model.Client) {
		client.Enable = false
	})
	initialRevision, _ := UserRevisionForInbound(f.inbound)
	operation, err := controller.ApplyUserConfig(
		context.Background(), f.inbound, next, initialRevision, "stop-last-user-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Hot || operation.PIDAfter != 0 {
		t.Fatalf("last-user operation = %#v", operation)
	}
	f.manager.mu.Lock()
	active := f.manager.snapshot.Active
	f.manager.mu.Unlock()
	if active {
		t.Fatal("fixed unit remained active after removing last enabled user")
	}
	if err := controller.ConfirmUserConfig(context.Background(), operation.OperationID); err != nil {
		t.Fatal(err)
	}
}

func TestUserOperationBootConfigWriteFailureRollsBackRuntime(t *testing.T) {
	f := newLifecycleFixture(t)
	controller, control := installTransactionControl(t, f)
	initialRevision, _ := UserRevisionForInbound(f.inbound)
	next := mutateGhostLinkClient(t, f.inbound, func(client *model.Client) {
		client.Secret = "ghostlink-boot-write-failure-secret-0032"
	})
	backup := f.cfg.configPath + ".saved"
	if err := os.Rename(f.cfg.configPath, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.cfg.configPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ApplyUserConfig(
		context.Background(), f.inbound, next, initialRevision, "boot-write-failure-1",
	); err == nil {
		t.Fatal("boot config write failure was not returned")
	}
	if control.revision != initialRevision {
		t.Fatalf("runtime revision after rollback = %s, want %s", control.revision, initialRevision)
	}
	if _, exists, err := controller.PendingUserConfig(context.Background()); err != nil || !exists {
		t.Fatalf("journal after failed rollback exists=%v err=%v", exists, err)
	}
	if err := os.Remove(f.cfg.configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, f.cfg.configPath); err != nil {
		t.Fatal(err)
	}
	resolution, err := controller.ResolvePendingUserConfig(context.Background(), initialRevision)
	if err != nil {
		t.Fatal(err)
	}
	if resolution != "rolled_back" {
		t.Fatalf("resolution = %q, want rolled_back", resolution)
	}
}

func TestUserOperationFirstEnabledUserStartsStoppedInstance(t *testing.T) {
	f := newLifecycleFixture(t)
	stopped := mutateGhostLinkClient(t, f.inbound, func(client *model.Client) {
		client.Enable = false
	})
	// Persist the zero-enabled state first so the fixture starts from stopped.
	f.inbound = stopped
	controller, control := installTransactionControlWithoutStart(t, f)
	next := mutateGhostLinkClient(t, stopped, func(client *model.Client) {
		client.Enable = true
	})
	oldRevision, _ := UserRevisionForInbound(stopped)
	newUsers := map[string]RuntimeUser{"alice-device": {
		Secret: testSecret, Enabled: true, PolicyVersion: 1,
		MaxConnections: -1, MaxTCPStreams: -1, MaxUDPRoutes: -1,
	}}
	newRevision := mustRuntimeUserRevision(t, newUsers)
	control.mu.Lock()
	control.users = cloneRuntimeUsers(newUsers)
	control.revision = newRevision
	control.mu.Unlock()
	operation, err := controller.ApplyUserConfig(
		context.Background(), stopped, next, "", "start-first-user-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Hot || operation.PIDAfter == 0 {
		t.Fatalf("first-user operation = %#v", operation)
	}
	if control.revision != operation.NewUserRevision {
		t.Fatalf("runtime revision = %s, want %s", control.revision, operation.NewUserRevision)
	}
	if oldRevision == operation.NewUserRevision {
		t.Fatal("enabling first user did not change revision")
	}
	if err := controller.ConfirmUserConfig(context.Background(), operation.OperationID); err != nil {
		t.Fatal(err)
	}
}

func installTransactionControlWithoutStart(
	t *testing.T,
	f lifecycleFixture,
) (*systemdController, *transactionFakeControl) {
	t.Helper()
	var settings Settings
	if err := json.Unmarshal([]byte(f.inbound.Settings), &settings); err != nil {
		t.Fatal(err)
	}
	users := make(map[string]RuntimeUser, len(settings.Clients))
	for _, client := range settings.Clients {
		users[client.ID] = RuntimeUser{
			Secret: client.Secret, Enabled: client.Enable,
			ExpiresAtUnixMilli: client.ExpiryTime, PolicyVersion: 1,
			MaxConnections: -1, MaxTCPStreams: -1, MaxUDPRoutes: -1,
		}
	}
	control := &transactionFakeControl{
		manager: f.manager, listen: "127.0.0.1:38443",
		peerUID: f.cfg.controlServerUID, users: users,
		revision: mustRuntimeUserRevision(t, users),
	}
	f.cfg.controlClient = control
	controller := newSystemdController(f.cfg, f.manager)
	return controller, control
}

func osReadFile(path string) ([]byte, error) {
	raw, _, err := readOptionalFile(path)
	return raw, err
}
