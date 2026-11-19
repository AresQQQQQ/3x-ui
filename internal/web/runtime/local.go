package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mhsanaei/3x-ui/v3/internal/amneziawg"
	"github.com/mhsanaei/3x-ui/v3/internal/amneziawgkern"
	"github.com/mhsanaei/3x-ui/v3/internal/amneziawgnet"
	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/logger"
	"github.com/mhsanaei/3x-ui/v3/internal/mtproto"
	"github.com/mhsanaei/3x-ui/v3/internal/tuic"
	"github.com/mhsanaei/3x-ui/v3/internal/xray"
)

type LocalDeps struct {
	APIPort          func() int
	SetNeedRestart   func()
	RequestReconcile func()
	GhostLink        ghostlink.Controller
}

type ghostLinkUsageRuntime interface {
	GhostLinkUsageSnapshot(ctx context.Context, inbound *model.Inbound) (ghostlink.UsageSnapshot, error)
}

type ghostLinkPolicyController interface {
	ReconcileGhostLinkWithPolicies(context.Context, *model.Inbound, map[string]ghostlink.RuntimePolicy) (ghostlink.Status, error)
	GhostLinkStatusWithPolicies(context.Context, *model.Inbound, map[string]ghostlink.RuntimePolicy) (ghostlink.Status, error)
	ApplyUserPolicyConfig(
		context.Context,
		*model.Inbound,
		*model.Inbound,
		map[string]ghostlink.RuntimePolicy,
		map[string]ghostlink.RuntimePolicy,
		string,
		string,
	) (ghostlink.UserOperation, error)
}

var ErrGhostLinkLifecycleUnsupported = errors.New(
	"GhostLink lifecycle controller is not configured",
)

var ErrGhostLinkUserHotSyncUnsupported = errors.New(
	"GhostLink user changes require the serialized single-client service transaction",
)

type Local struct {
	deps LocalDeps
	mu   sync.Mutex
}

func NewLocal(deps LocalDeps) *Local {
	return &Local{deps: deps}
}

func (l *Local) Name() string { return "local" }

func (l *Local) withAPI(fn func(api *xray.XrayAPI) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	port := l.deps.APIPort()
	if port <= 0 {
		return errors.New("local xray is not running")
	}
	api, err := localAPIForPort(port)
	if err != nil {
		return err
	}
	err = fn(api)
	if status.Code(err) != codes.Unavailable {
		return err
	}
	// A pooled channel keeps grpc-go's reconnect backoff after a core restart and fails fast for
	// seconds; drop it and dial again, as a client per call did.
	evictLocalAPIClient(api)
	api, err = localAPIForPort(port)
	if err != nil {
		return err
	}
	return fn(api)
}

func (l *Local) AddInbound(ctx context.Context, ib *model.Inbound) error {
	if ib.Protocol == model.GhostLink {
		_, err := l.ReconcileGhostLink(ctx, ib)
		return err
	}
	if ib.Protocol == model.MTProto {
		inst, ok := mtproto.InstanceFromInbound(ib)
		if !ok {
			return nil
		}
		return mtproto.GetManager().Ensure(inst)
	}
	if ib.Protocol == model.AmneziaWG {
		inst, ok := amneziawg.InstanceFromInbound(ib)
		if !ok {
			return nil
		}
		err := ensureAmneziaWG(inst)
		// A brand new inbound can be the first one to qualify for
		// injectAmneziawgnetSocks's Xray-side relay inbound (e.g. its first
		// valid peer). Ensure only updates the embedded Device -- flag Xray
		// for a resync so the relay actually gets created within the next
		// ApplyPendingRestart tick instead of only at the next full restart.
		if l.deps.SetNeedRestart != nil {
			l.deps.SetNeedRestart()
		}
		return err
	}
	if ib.Protocol == model.TUIC {
		inst, ok := tuic.InstanceFromInbound(ib)
		if !ok {
			return nil
		}
		err := tuic.GetManager().Ensure(inst)
		if l.deps.RequestReconcile != nil {
			l.deps.RequestReconcile()
		}
		return err
	}
	body, err := json.MarshalIndent(ib.GenXrayInboundConfig(), "", "  ")
	if err != nil {
		return err
	}
	return l.withAPI(func(api *xray.XrayAPI) error {
		return api.AddInbound(body)
	})
}

func (l *Local) DelInbound(ctx context.Context, ib *model.Inbound) error {
	if ib.Protocol == model.GhostLink {
		_, err := l.StopGhostLink(ctx)
		return err
	}
	if ib.Protocol == model.MTProto {
		mtproto.GetManager().Remove(ib.Id)
		return nil
	}
	if ib.Protocol == model.AmneziaWG {
		removeAmneziaWG(ib.Id)
		// The removed inbound may have been the only one backing Xray's
		// injectAmneziawgnetSocks relay inbound for this tag -- flag a
		// resync so the now-stale relay gets torn down promptly.
		if l.deps.SetNeedRestart != nil {
			l.deps.SetNeedRestart()
		}
		return nil
	}
	if ib.Protocol == model.TUIC {
		tuic.GetManager().Remove(ib.Id)
		if l.deps.RequestReconcile != nil {
			l.deps.RequestReconcile()
		}
		return nil
	}
	return l.withAPI(func(api *xray.XrayAPI) error {
		return api.DelInbound(ib.Tag)
	})
}

func (l *Local) UpdateInbound(ctx context.Context, oldIb, newIb *model.Inbound) error {
	if oldIb.Protocol == model.GhostLink || newIb.Protocol == model.GhostLink {
		_, err := l.ReconcileGhostLink(ctx, newIb)
		return err
	}
	if oldIb.Protocol == model.MTProto || newIb.Protocol == model.MTProto {
		return l.updateMtprotoInbound(ctx, oldIb, newIb)
	}
	if oldIb.Protocol == model.AmneziaWG || newIb.Protocol == model.AmneziaWG {
		return l.updateAmneziaWGInbound(ctx, oldIb, newIb)
	}
	if oldIb.Protocol == model.TUIC || newIb.Protocol == model.TUIC {
		return l.updateTuicInbound(ctx, oldIb, newIb)
	}
	_ = l.DelInbound(ctx, oldIb)
	if !newIb.Enable {
		return nil
	}
	return l.AddInbound(ctx, newIb)
}

// updateMtprotoInbound applies an inbound update without the Del+Add sequence
// the xray path uses: Remove would drop the manager's fingerprint state, which
// is what lets Ensure keep the running mtg process (and its live connections)
// when nothing in the generated config changed. The sidecar is only stopped
// when the inbound is disabled, loses its last active secret, or moves to a
// different protocol.
func (l *Local) updateMtprotoInbound(ctx context.Context, oldIb, newIb *model.Inbound) error {
	if oldIb.Protocol == model.MTProto && newIb.Protocol != model.MTProto {
		mtproto.GetManager().Remove(oldIb.Id)
		if !newIb.Enable {
			return nil
		}
		return l.AddInbound(ctx, newIb)
	}
	if oldIb.Protocol != model.MTProto {
		_ = l.DelInbound(ctx, oldIb)
	}
	if !newIb.Enable {
		mtproto.GetManager().Remove(newIb.Id)
		return nil
	}
	inst, ok := mtproto.InstanceFromInbound(newIb)
	if !ok {
		mtproto.GetManager().Remove(newIb.Id)
		return nil
	}
	return mtproto.GetManager().Ensure(inst)
}

// updateAmneziaWGInbound routes AmneziaWG CRUD through the active datapath managers.
func (l *Local) updateAmneziaWGInbound(ctx context.Context, oldIb, newIb *model.Inbound) error {
	if l.deps.SetNeedRestart != nil {
		l.deps.SetNeedRestart()
	}
	if oldIb.Protocol == model.AmneziaWG && newIb.Protocol != model.AmneziaWG {
		removeAmneziaWG(oldIb.Id)
		if !newIb.Enable {
			return nil
		}
		return l.AddInbound(ctx, newIb)
	}
	if oldIb.Protocol != model.AmneziaWG {
		_ = l.DelInbound(ctx, oldIb)
	}
	if !newIb.Enable {
		removeAmneziaWG(newIb.Id)
		return nil
	}
	inst, ok := amneziawg.InstanceFromInbound(newIb)
	if !ok {
		removeAmneziaWG(newIb.Id)
		return nil
	}
	return ensureAmneziaWG(inst)
}

// ensureAmneziaWG routes an inbound to its own datapath's manager and clears
// the other's: both bind the inbound's UDP port, so they must never overlap.
func ensureAmneziaWG(inst amneziawg.Instance) error {
	if inst.KernelMode {
		amneziawgnet.GetManager().Remove(inst.Id)
		return amneziawgkern.GetManager().Ensure(amneziawgkern.Desired{
			Instance:         inst,
			EgressInterface:  inst.ExternalInterface,
			RouteThroughXray: inst.RouteThroughXray,
		})
	}
	if err := amneziawgkern.GetManager().Remove(inst.Id); err != nil {
		return err
	}
	return amneziawgnet.GetManager().Ensure(amneziawgnet.Desired{
		Instance: inst,
		Options: amneziawgnet.DeviceOptions{
			HeaderProtectionKey:    inst.Obfuscation.HeaderProtectionKey,
			ContentPaddingAddition: inst.Obfuscation.ContentPaddingAddition,
			RekeyAfterTime:         inst.Obfuscation.RekeyAfterTime,
			RekeyTimeout:           inst.Obfuscation.RekeyTimeout,
			RejectAfterTime:        inst.Obfuscation.RejectAfterTime,
			KeepaliveTimeout:       inst.Obfuscation.KeepaliveTimeout,
			MaxHandshakeAttempts:   inst.Obfuscation.MaxHandshakeAttempts,
			RandomTrailers:         inst.Obfuscation.RandomTrailers,
			DisableCookies:         inst.Obfuscation.DisableCookies,
		},
	})
}

// removeAmneziaWG tears the inbound down on whichever datapath is holding it.
func removeAmneziaWG(id int) {
	amneziawgnet.GetManager().Remove(id)
	if err := amneziawgkern.GetManager().Remove(id); err != nil {
		logger.Warning("amneziawgkern: removing inbound ", id, ": ", err)
	}
}

func (l *Local) updateTuicInbound(ctx context.Context, oldIb, newIb *model.Inbound) error {
	// Account changes also need the generated Xray SOCKS relay refreshed after commit.
	if l.deps.RequestReconcile != nil {
		l.deps.RequestReconcile()
	}
	if oldIb.Protocol == model.TUIC && newIb.Protocol != model.TUIC {
		tuic.GetManager().Remove(oldIb.Id)
		if !newIb.Enable {
			return nil
		}
		return l.AddInbound(ctx, newIb)
	}
	if oldIb.Protocol != model.TUIC {
		_ = l.DelInbound(ctx, oldIb)
	}
	if !newIb.Enable {
		tuic.GetManager().Remove(newIb.Id)
		return nil
	}
	inst, ok := tuic.InstanceFromInbound(newIb)
	if !ok {
		tuic.GetManager().Remove(newIb.Id)
		return nil
	}
	return tuic.GetManager().Ensure(inst)
}

// ReconcileGhostLink updates the one fixed OS-supervised GhostLink instance.
// The controller owns all unit/path choices; Inbound fields can only provide
// validated GhostLink protocol configuration.
func (l *Local) ReconcileGhostLink(ctx context.Context, ib *model.Inbound) (ghostlink.Status, error) {
	if l.deps.GhostLink == nil {
		return ghostlink.Status{}, ErrGhostLinkLifecycleUnsupported
	}
	return l.deps.GhostLink.Reconcile(ctx, ib)
}

func (l *Local) StopGhostLink(ctx context.Context) (ghostlink.Status, error) {
	if l.deps.GhostLink == nil {
		return ghostlink.Status{}, ErrGhostLinkLifecycleUnsupported
	}
	return l.deps.GhostLink.Stop(ctx)
}

func (l *Local) GhostLinkStatus(ctx context.Context, ib *model.Inbound) (ghostlink.Status, error) {
	if l.deps.GhostLink == nil {
		return ghostlink.Status{}, ErrGhostLinkLifecycleUnsupported
	}
	return l.deps.GhostLink.Status(ctx, ib)
}

func (l *Local) GhostLinkUsageSnapshot(ctx context.Context, ib *model.Inbound) (ghostlink.UsageSnapshot, error) {
	if l.deps.GhostLink == nil {
		return ghostlink.UsageSnapshot{}, ErrGhostLinkLifecycleUnsupported
	}
	usage, ok := l.deps.GhostLink.(ghostLinkUsageRuntime)
	if !ok {
		return ghostlink.UsageSnapshot{}, errors.New("GhostLink controller does not support usage snapshots")
	}
	return usage.GhostLinkUsageSnapshot(ctx, ib)
}

func (l *Local) ReconcileGhostLinkWithPolicies(
	ctx context.Context,
	ib *model.Inbound,
	policies map[string]ghostlink.RuntimePolicy,
) (ghostlink.Status, error) {
	controller, err := l.ghostLinkPolicyController()
	if err != nil {
		return ghostlink.Status{}, err
	}
	return controller.ReconcileGhostLinkWithPolicies(ctx, ib, policies)
}

func (l *Local) GhostLinkStatusWithPolicies(
	ctx context.Context,
	ib *model.Inbound,
	policies map[string]ghostlink.RuntimePolicy,
) (ghostlink.Status, error) {
	controller, err := l.ghostLinkPolicyController()
	if err != nil {
		return ghostlink.Status{}, err
	}
	return controller.GhostLinkStatusWithPolicies(ctx, ib, policies)
}

func (l *Local) ApplyGhostLinkUserPolicyConfig(
	ctx context.Context,
	oldInbound *model.Inbound,
	newInbound *model.Inbound,
	oldPolicies map[string]ghostlink.RuntimePolicy,
	newPolicies map[string]ghostlink.RuntimePolicy,
	expectedRuntimeRevision string,
	requestID string,
) (ghostlink.UserOperation, error) {
	controller, err := l.ghostLinkPolicyController()
	if err != nil {
		return ghostlink.UserOperation{}, err
	}
	return controller.ApplyUserPolicyConfig(
		ctx,
		oldInbound,
		newInbound,
		oldPolicies,
		newPolicies,
		expectedRuntimeRevision,
		requestID,
	)
}

func (l *Local) ghostLinkPolicyController() (ghostLinkPolicyController, error) {
	if l.deps.GhostLink == nil {
		return nil, ErrGhostLinkLifecycleUnsupported
	}
	controller, ok := l.deps.GhostLink.(ghostLinkPolicyController)
	if !ok {
		return nil, errors.New("GhostLink controller does not support runtime policy updates")
	}
	return controller, nil
}

func (l *Local) ApplyGhostLinkUserConfig(
	ctx context.Context,
	oldInbound *model.Inbound,
	newInbound *model.Inbound,
	expectedRuntimeRevision string,
	requestID string,
) (ghostlink.UserOperation, error) {
	controller, err := l.ghostLinkUserOperationController()
	if err != nil {
		return ghostlink.UserOperation{}, err
	}
	return controller.ApplyUserConfig(ctx, oldInbound, newInbound, expectedRuntimeRevision, requestID)
}

func (l *Local) ConfirmGhostLinkUserConfig(ctx context.Context, operationID string) error {
	controller, err := l.ghostLinkUserOperationController()
	if err != nil {
		return err
	}
	return controller.ConfirmUserConfig(ctx, operationID)
}

func (l *Local) RollbackGhostLinkUserConfig(ctx context.Context, operationID string) error {
	controller, err := l.ghostLinkUserOperationController()
	if err != nil {
		return err
	}
	return controller.RollbackUserConfig(ctx, operationID)
}

func (l *Local) ResolvePendingGhostLinkUserConfig(ctx context.Context, desiredUserRevision string) (string, error) {
	controller, err := l.ghostLinkUserOperationController()
	if err != nil {
		return "", err
	}
	return controller.ResolvePendingUserConfig(ctx, desiredUserRevision)
}

func (l *Local) PendingGhostLinkUserConfig(ctx context.Context) (ghostlink.PendingUserOperation, bool, error) {
	controller, err := l.ghostLinkUserOperationController()
	if err != nil {
		return ghostlink.PendingUserOperation{}, false, err
	}
	return controller.PendingUserConfig(ctx)
}

func (l *Local) ghostLinkUserOperationController() (ghostlink.UserOperationController, error) {
	if l.deps.GhostLink == nil {
		return nil, ErrGhostLinkLifecycleUnsupported
	}
	controller, ok := l.deps.GhostLink.(ghostlink.UserOperationController)
	if !ok {
		return nil, errors.New("GhostLink controller does not support transactional user updates")
	}
	return controller, nil
}

func (l *Local) AddUser(_ context.Context, ib *model.Inbound, userMap map[string]any) error {
	if ib.Protocol == model.GhostLink {
		return ErrGhostLinkUserHotSyncUnsupported
	}
	if ib.Protocol == model.TUIC {
		if l.deps.RequestReconcile != nil {
			l.deps.RequestReconcile()
		}
		return nil
	}
	if ib.Protocol == model.MTProto || ib.Protocol == model.AmneziaWG {
		return nil
	}
	return l.withAPI(func(api *xray.XrayAPI) error {
		return api.AddUser(string(ib.Protocol), ib.Tag, userMap)
	})
}

func (l *Local) RemoveUser(_ context.Context, ib *model.Inbound, email string) error {
	if ib.Protocol == model.GhostLink {
		return ErrGhostLinkUserHotSyncUnsupported
	}
	if ib.Protocol == model.TUIC {
		if l.deps.RequestReconcile != nil {
			l.deps.RequestReconcile()
		}
		return nil
	}
	if ib.Protocol == model.MTProto || ib.Protocol == model.AmneziaWG {
		return nil
	}
	return l.withAPI(func(api *xray.XrayAPI) error {
		return api.RemoveUser(ib.Tag, email)
	})
}

func (l *Local) AddClient(ctx context.Context, ib *model.Inbound, client model.Client) error {
	if !client.Enable {
		return nil
	}
	user := map[string]any{
		"email":        client.Email,
		"id":           client.ID,
		"security":     client.Security,
		"flow":         client.Flow,
		"auth":         client.Auth,
		"password":     client.Password,
		"publicKey":    client.PublicKey,
		"allowedIPs":   client.AllowedIPs,
		"preSharedKey": client.PreSharedKey,
		"keepAlive":    wgKeepAlive(client.KeepAliveSeconds()),
	}
	return l.AddUser(ctx, ib, user)
}

func (l *Local) DeleteUser(ctx context.Context, ib *model.Inbound, email string) error {
	if email == "" {
		return nil
	}
	if err := l.RemoveUser(ctx, ib, email); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
		return err
	}
	return nil
}

func (l *Local) DeleteClient(context.Context, string) error {
	return nil
}

func (l *Local) UpdateUser(ctx context.Context, ib *model.Inbound, oldEmail string, payload model.Client) error {
	if oldEmail != "" {
		if err := l.RemoveUser(ctx, ib, oldEmail); err != nil && !strings.Contains(err.Error(), "not found") {
			return err
		}
	}
	if !payload.Enable {
		return nil
	}
	user := map[string]any{
		"email":        payload.Email,
		"id":           payload.ID,
		"security":     payload.Security,
		"flow":         payload.Flow,
		"auth":         payload.Auth,
		"password":     payload.Password,
		"publicKey":    payload.PublicKey,
		"allowedIPs":   payload.AllowedIPs,
		"preSharedKey": payload.PreSharedKey,
		"keepAlive":    wgKeepAlive(payload.KeepAliveSeconds()),
	}
	return l.AddUser(ctx, ib, user)
}

func wgKeepAlive(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	return strconv.Itoa(seconds)
}

func (l *Local) RestartXray(_ context.Context) error {
	if l.deps.SetNeedRestart != nil {
		l.deps.SetNeedRestart()
	}
	return nil
}

func (l *Local) ResetClientTraffic(_ context.Context, _ *model.Inbound, _ string) error {
	return nil
}

func (l *Local) ResetAllTraffics(_ context.Context) error {
	return nil
}

func (l *Local) ResetInboundTraffic(_ context.Context, _ *model.Inbound) error {
	return nil
}
