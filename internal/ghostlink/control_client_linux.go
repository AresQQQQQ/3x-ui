//go:build linux

package ghostlink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

const maxControlResponseBytes = 1 << 20

type unixControlClient struct {
	path              string
	expectedServerUID uint32
}

func newLocalControlClient(path string, expectedServerUID uint32) localControlClient {
	return &unixControlClient{path: path, expectedServerUID: expectedServerUID}
}

func (c *unixControlClient) Status(ctx context.Context) (controlResponse, error) {
	return c.call(ctx, controlRequest{Op: "status"})
}

func (c *unixControlClient) Usage(ctx context.Context) (controlResponse, error) {
	return c.call(ctx, controlRequest{Op: "usage_snapshot"})
}

func (c *unixControlClient) ApplyUsers(
	ctx context.Context,
	requestID string,
	expectedRevision string,
	users map[string]RuntimeUser,
) (controlResponse, error) {
	return c.call(ctx, controlRequest{
		Op:               "apply_users",
		RequestID:        requestID,
		ExpectedRevision: expectedRevision,
		Users:            users,
	})
}

func (c *unixControlClient) call(ctx context.Context, request controlRequest) (controlResponse, error) {
	if c == nil || c.path == "" {
		return controlResponse{}, errors.New("GhostLink local control socket is not configured")
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.path)
	if err != nil {
		return controlResponse{}, fmt.Errorf("dial GhostLink local control: %w", err)
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return controlResponse{}, errors.New("local control connection is not a Unix socket")
	}
	defer unixConn.Close()
	_ = unixConn.SetDeadline(time.Now().Add(5 * time.Second))

	peerPID, peerUID, _, err := unixPeerCredentials(unixConn)
	if err != nil {
		return controlResponse{}, err
	}
	if peerUID != c.expectedServerUID {
		return controlResponse{}, fmt.Errorf("GhostLink local control peer UID %d does not match expected UID %d", peerUID, c.expectedServerUID)
	}

	raw, err := json.Marshal(request)
	if err != nil {
		return controlResponse{}, err
	}
	if _, err := unixConn.Write(append(raw, '\n')); err != nil {
		return controlResponse{}, fmt.Errorf("write GhostLink local control request: %w", err)
	}
	responseRaw, err := bufio.NewReaderSize(unixConn, 32*1024).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return controlResponse{}, fmt.Errorf("read GhostLink local control response: %w", err)
	}
	if len(responseRaw) == 0 || len(responseRaw) > maxControlResponseBytes {
		return controlResponse{}, errors.New("GhostLink local control response size is invalid")
	}
	responseRaw = bytes.TrimSpace(responseRaw)
	decoder := json.NewDecoder(bytes.NewReader(responseRaw))
	decoder.DisallowUnknownFields()
	var response controlResponse
	if err := decoder.Decode(&response); err != nil {
		return controlResponse{}, fmt.Errorf("decode GhostLink local control response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return controlResponse{}, errors.New("GhostLink local control response contains multiple JSON values")
	}
	response.PeerPID = peerPID
	response.PeerUID = peerUID
	if !response.OK {
		code := response.ErrorCode
		if code == "" {
			code = "control_error"
		}
		return response, fmt.Errorf("GhostLink local control rejected request (%s): %s", code, response.Error)
	}
	return response, nil
}

func unixPeerCredentials(conn *net.UnixConn) (pid int, uid uint32, gid uint32, err error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, 0, err
	}
	if credErr != nil {
		return 0, 0, 0, credErr
	}
	return int(cred.Pid), cred.Uid, cred.Gid, nil
}
