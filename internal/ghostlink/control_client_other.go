//go:build !linux

package ghostlink

import (
	"context"
	"errors"
)

type unsupportedControlClient struct{}

func newLocalControlClient(string, uint32) localControlClient {
	return unsupportedControlClient{}
}

func (unsupportedControlClient) Status(context.Context) (controlResponse, error) {
	return controlResponse{}, errors.New("GhostLink local control is supported only on Linux")
}

func (unsupportedControlClient) Usage(context.Context) (controlResponse, error) {
	return controlResponse{}, errors.New("GhostLink local control is supported only on Linux")
}

func (unsupportedControlClient) ApplyUsers(
	context.Context,
	string,
	string,
	map[string]RuntimeUser,
) (controlResponse, error) {
	return controlResponse{}, errors.New("GhostLink local control is supported only on Linux")
}
