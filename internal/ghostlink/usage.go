package ghostlink

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
)

type UsageUser struct {
	ClientID          string
	UploadBytes       uint64
	DownloadBytes     uint64
	PersistenceFailed bool
	UpdatedAtUnixNano int64
}

type UsageSnapshot struct {
	StoreID             string
	InstanceID          string
	GeneratedAtUnixNano int64
	Source              string
	Users               []UsageUser
}

type UsageController interface {
	GhostLinkUsageSnapshot(context.Context, *model.Inbound) (UsageSnapshot, error)
}

func (c *systemdController) GhostLinkUsageSnapshot(ctx context.Context, inbound *model.Inbound) (UsageSnapshot, error) {
	if inbound != nil && inbound.Protocol != model.GhostLink {
		return UsageSnapshot{}, errors.New("not a GhostLink inbound")
	}
	snapshot, err := c.manager.Snapshot(ctx)
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("read GhostLink systemd state for usage: %w", err)
	}
	if snapshot.Loaded && snapshot.Active {
		if c.cfg.controlClient == nil {
			return UsageSnapshot{}, errors.New("GhostLink local control client is not configured")
		}
		status, statusErr := c.cfg.controlClient.Status(ctx)
		if statusErr != nil {
			return UsageSnapshot{}, statusErr
		}
		usage, usageErr := c.cfg.controlClient.Usage(ctx)
		if usageErr != nil {
			return UsageSnapshot{}, usageErr
		}
		if usage.PeerPID != snapshot.PID || usage.PID != snapshot.PID ||
			status.PeerPID != snapshot.PID || status.PID != snapshot.PID {
			return UsageSnapshot{}, errors.New("GhostLink usage snapshot peer PID does not match systemd MainPID")
		}
		if usage.PeerUID != c.cfg.controlServerUID || status.PeerUID != c.cfg.controlServerUID {
			return UsageSnapshot{}, errors.New("GhostLink usage snapshot peer UID does not match the fixed service identity")
		}
		if !usage.Serving || !status.Serving {
			return UsageSnapshot{}, errors.New("GhostLink runtime stopped while reading usage")
		}
		if usage.InstanceID == "" || usage.InstanceID != status.InstanceID {
			return UsageSnapshot{}, errors.New("GhostLink usage snapshot instance does not match readiness status")
		}
		if usage.Usage == nil || usage.Usage.StoreID == "" ||
			usage.Usage.InstanceID != usage.InstanceID {
			return UsageSnapshot{}, errors.New("GhostLink usage snapshot identity is incomplete")
		}
		users := make([]UsageUser, 0, len(usage.Usage.Users))
		for _, user := range usage.Usage.Users {
			users = append(users, UsageUser{
				ClientID:          user.ClientID,
				UploadBytes:       user.UploadBytes,
				DownloadBytes:     user.DownloadBytes,
				PersistenceFailed: user.PersistenceFailed,
				UpdatedAtUnixNano: user.UpdatedAtUnixNano,
			})
		}
		return UsageSnapshot{
			StoreID:             usage.Usage.StoreID,
			InstanceID:          usage.Usage.InstanceID,
			GeneratedAtUnixNano: usage.Usage.GeneratedAtUnixNano,
			Source:              "uds",
			Users:               users,
		}, nil
	}
	stateSnapshot, err := readUsageStateDatabase(ctx, c.cfg.stateDBPath)
	if err != nil {
		if errors.Is(err, errUsageStateUnavailable) {
			return UsageSnapshot{
				StoreID:             "uninitialized",
				GeneratedAtUnixNano: time.Now().UnixNano(),
				Source:              "sqlite",
			}, nil
		}
		return UsageSnapshot{}, err
	}
	stateSnapshot.Source = "sqlite"
	return stateSnapshot, nil
}
