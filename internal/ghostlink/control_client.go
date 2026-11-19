package ghostlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type RuntimeUser struct {
	Secret             string `json:"secret"`
	Enabled            bool   `json:"enabled"`
	MaxConnections     int    `json:"max_connections,omitempty"`
	MaxTCPStreams      int    `json:"max_tcp_streams,omitempty"`
	MaxUDPRoutes       int    `json:"max_udp_routes,omitempty"`
	MaxTrafficBytes    uint64 `json:"max_traffic_bytes,omitempty"`
	ExpiresAtUnixMilli int64  `json:"expires_at_unix_milli,omitempty"`
	PolicyVersion      int    `json:"policy_version,omitempty"`
}

type RuntimeUsageUser struct {
	ClientID          string `json:"client_id"`
	UploadBytes       uint64 `json:"upload_bytes"`
	DownloadBytes     uint64 `json:"download_bytes"`
	PersistenceFailed bool   `json:"persistence_failed"`
	UpdatedAtUnixNano int64  `json:"updated_at_unix_nano,omitempty"`
}

type RuntimeUsageSnapshot struct {
	StoreID             string             `json:"store_id"`
	InstanceID          string             `json:"instance_id"`
	GeneratedAtUnixNano int64              `json:"generated_at_unix_nano"`
	Users               []RuntimeUsageUser `json:"users"`
}

type controlRequest struct {
	Op               string                 `json:"op"`
	RequestID        string                 `json:"request_id,omitempty"`
	ExpectedRevision string                 `json:"expected_revision,omitempty"`
	Users            map[string]RuntimeUser `json:"users,omitempty"`
}

type controlResponse struct {
	OK           bool                  `json:"ok"`
	ErrorCode    string                `json:"error_code,omitempty"`
	Error        string                `json:"error,omitempty"`
	PID          int                   `json:"pid,omitempty"`
	InstanceID   string                `json:"instance_id,omitempty"`
	Serving      bool                  `json:"serving"`
	H2Listen     string                `json:"h2_listen,omitempty"`
	H3Listen     string                `json:"h3_listen,omitempty"`
	ActiveUsers  int                   `json:"active_users,omitempty"`
	UserRevision string                `json:"user_revision,omitempty"`
	Usage        *RuntimeUsageSnapshot `json:"usage,omitempty"`
	PeerPID      int                   `json:"-"`
	PeerUID      uint32                `json:"-"`
}

type localControlClient interface {
	Status(context.Context) (controlResponse, error)
	Usage(context.Context) (controlResponse, error)
	ApplyUsers(context.Context, string, string, map[string]RuntimeUser) (controlResponse, error)
}

func RuntimeUsersRevision(users map[string]RuntimeUser) (string, error) {
	encoded, err := json.Marshal(users)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
