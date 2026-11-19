package ghostlink

import (
	"context"

	"github.com/mhsanaei/3x-ui/v3/internal/database/model"
)

// RuntimePolicyController exposes policy-aware variants of the existing
// lifecycle and user-operation interfaces without changing the round-3/4
// standalone contracts.
type RuntimePolicyController interface {
	ReconcileGhostLinkWithPolicies(context.Context, *model.Inbound, map[string]RuntimePolicy) (Status, error)
	GhostLinkStatusWithPolicies(context.Context, *model.Inbound, map[string]RuntimePolicy) (Status, error)
	ApplyUserPolicyConfig(
		context.Context,
		*model.Inbound,
		*model.Inbound,
		map[string]RuntimePolicy,
		map[string]RuntimePolicy,
		string,
		string,
	) (UserOperation, error)
}
