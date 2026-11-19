//go:build !linux

package ghostlink

import (
	"context"
	"errors"
)

var errUsageStateUnavailable = errors.New("GhostLink usage state database is unavailable")

func readUsageStateDatabase(context.Context, string) (UsageSnapshot, error) {
	return UsageSnapshot{}, errors.New("GhostLink usage state database is supported only on Linux")
}
