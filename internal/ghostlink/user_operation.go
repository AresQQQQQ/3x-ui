package ghostlink

import (
	"context"

	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
)

const DefaultUserOperationJournal = "/etc/ghostlinkd-3xui/pending-user-op.json"

type UserOperation struct {
	OperationID     string
	RequestID       string
	OldUserRevision string
	NewUserRevision string
	Hot             bool
	PIDBefore       int
	PIDAfter        int
	Status          Status
}

type PendingUserOperation struct {
	OperationID     string `json:"operation_id"`
	RequestID       string `json:"request_id"`
	Phase           string `json:"phase"`
	OldUserRevision string `json:"old_user_revision"`
	NewUserRevision string `json:"new_user_revision"`
	OldShouldRun    bool   `json:"old_should_run"`
	NewShouldRun    bool   `json:"new_should_run"`
	Hot             bool   `json:"hot"`
	PIDBefore       int    `json:"pid_before,omitempty"`
}

type UserOperationController interface {
	ApplyUserConfig(
		ctx context.Context,
		oldInbound *model.Inbound,
		newInbound *model.Inbound,
		expectedRuntimeRevision string,
		requestID string,
	) (UserOperation, error)
	ConfirmUserConfig(ctx context.Context, operationID string) error
	RollbackUserConfig(ctx context.Context, operationID string) error
	ResolvePendingUserConfig(ctx context.Context, desiredUserRevision string) (string, error)
	PendingUserConfig(ctx context.Context) (PendingUserOperation, bool, error)
}
