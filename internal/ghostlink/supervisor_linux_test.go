package ghostlink

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerProcessManager(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "core-test")
	script := []byte("#!/bin/sh\nprintf '%s\\n%s\\n' \"$GODEBUG\" \"$GOMEMLIMIT\" > \"$2\"\ntrap 'exit 0' TERM\nwhile :; do sleep 0.05; done\n")
	if err := os.WriteFile(path, script, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODEBUG", "madvdontneed=1,http2xconnect=0")
	t.Setenv("GHOSTLINK_GOMEMLIMIT", "85MiB")
	m := &processManager{binary: path, config: filepath.Join(dir, "env")}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	defer m.DisableNow(context.Background())
	initial, err := m.Snapshot(ctx)
	if err != nil || initial.Active || m.Available() {
		t.Fatalf("new supervisor launched a process: %+v %v", initial, err)
	}
	if err := m.EnableNow(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := m.Snapshot(ctx)
	if err != nil || !first.Active || first.PID <= 0 {
		t.Fatalf("no active process: %+v %v", first, err)
	}
	for {
		if env, err := os.ReadFile(m.config); err == nil && strings.TrimSpace(string(env)) == "madvdontneed=1,http2xconnect=1\n85MiB" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("child did not receive configured Go runtime variables")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := m.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := m.Snapshot(ctx)
	if err != nil || !second.Active || second.PID == first.PID {
		t.Fatalf("restart did not replace child: %+v %v", second, err)
	}
	if err := m.DisableNow(ctx); err != nil {
		t.Fatal(err)
	}
	last, err := m.Snapshot(ctx)
	if err != nil || last.Active || m.Available() {
		t.Fatalf("child remained active: %+v %v", last, err)
	}
}
