package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
)

type ghostLinkControllerProbe struct {
	reconciles int
	stops      int
	statuses   int
	lastListen string
}

func (p *ghostLinkControllerProbe) Reconcile(_ context.Context, inbound *model.Inbound) (ghostlink.Status, error) {
	p.reconciles++
	p.lastListen = inbound.Listen
	return ghostlink.Status{Expected: inbound.Enable, Running: true, Reason: "running"}, nil
}

func (p *ghostLinkControllerProbe) Stop(context.Context) (ghostlink.Status, error) {
	p.stops++
	return ghostlink.Status{Reason: "stopped"}, nil
}

func (p *ghostLinkControllerProbe) Status(context.Context, *model.Inbound) (ghostlink.Status, error) {
	p.statuses++
	return ghostlink.Status{Reason: "running"}, nil
}

func TestLocalRoutesGhostLinkLifecycleWithoutFallingBackToXray(t *testing.T) {
	probe := &ghostLinkControllerProbe{}
	local := NewLocal(LocalDeps{GhostLink: probe})
	inbound := &model.Inbound{
		Id:       1,
		Protocol: model.GhostLink,
		Enable:   true,
		Listen:   "127.0.0.1",
		Port:     38443,
		Settings: `{"clients":[]}`,
	}
	if err := local.AddInbound(context.Background(), inbound); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	if err := local.UpdateInbound(context.Background(), inbound, inbound); err != nil {
		t.Fatalf("UpdateInbound: %v", err)
	}
	if _, err := local.GhostLinkStatus(context.Background(), inbound); err != nil {
		t.Fatalf("GhostLinkStatus: %v", err)
	}
	if err := local.DelInbound(context.Background(), inbound); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}
	if probe.reconciles != 2 || probe.stops != 1 || probe.statuses != 1 {
		t.Fatalf("controller calls reconcile=%d stop=%d status=%d", probe.reconciles, probe.stops, probe.statuses)
	}
	if probe.lastListen != "127.0.0.1" {
		t.Fatalf("controller last listen = %q", probe.lastListen)
	}

	if err := local.AddUser(context.Background(), inbound, map[string]any{"id": "alice"}); !errors.Is(err, ErrGhostLinkUserHotSyncUnsupported) {
		t.Fatalf("AddUser error = %v, want hot-sync unsupported", err)
	}
	if err := local.RemoveUser(context.Background(), inbound, "alice@example.test"); !errors.Is(err, ErrGhostLinkUserHotSyncUnsupported) {
		t.Fatalf("RemoveUser error = %v, want hot-sync unsupported", err)
	}
}

func TestLocalGhostLinkLifecycleRequiresController(t *testing.T) {
	local := NewLocal(LocalDeps{})
	inbound := &model.Inbound{Protocol: model.GhostLink}
	if err := local.AddInbound(context.Background(), inbound); !errors.Is(err, ErrGhostLinkLifecycleUnsupported) {
		t.Fatalf("AddInbound error = %v", err)
	}
}
