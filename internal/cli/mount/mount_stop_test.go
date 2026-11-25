package mount

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWaitForMountStopReturnsOnContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForMountStop(ctx, make(chan struct{})); err != nil {
		t.Fatalf("waitForMountStop() error = %v, want nil", err)
	}
}

func TestWaitForMountStopReportsSessionEnd(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	close(done)
	err := waitForMountStop(context.Background(), done)
	if !errors.Is(err, errMountSessionEnded) {
		t.Fatalf("waitForMountStop() error = %v, want errMountSessionEnded", err)
	}
}

func TestWaitForMountStopPrefersRequestedShutdown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	close(done)
	if err := waitForMountStop(ctx, done); err != nil {
		t.Fatalf("waitForMountStop() error = %v, want nil when shutdown was requested", err)
	}
}

func TestAwaitDirectMountStopExternalUnmountExitsZero(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	close(done)
	var errBuf bytes.Buffer
	unmounted := false
	err := awaitDirectMountStop(
		context.Background(),
		"/mnt/qrypt",
		done,
		func() error { return errors.New("fuse stopped") },
		func(string) bool { return false },
		func() { unmounted = true },
		&errBuf,
	)
	if err != nil {
		t.Fatalf("awaitDirectMountStop() = %v, want nil on external unmount", err)
	}
	if !unmounted {
		t.Fatal("unmount was not called")
	}
	if !strings.Contains(errBuf.String(), "Unmounted externally") {
		t.Fatalf("stderr = %q, want Unmounted externally notice", errBuf.String())
	}
}

func TestAwaitDirectMountStopUnexpectedDeathExitsNonZero(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	close(done)
	var errBuf bytes.Buffer
	unmounted := false
	err := awaitDirectMountStop(
		context.Background(),
		"/mnt/qrypt",
		done,
		func() error { return errors.New("short read on fuse device") },
		func(string) bool { return true },
		func() { unmounted = true },
		&errBuf,
	)
	if !errors.Is(err, errMountSessionEnded) {
		t.Fatalf("awaitDirectMountStop() = %v, want errMountSessionEnded", err)
	}
	if !unmounted {
		t.Fatal("unmount was not called")
	}
	if !strings.Contains(errBuf.String(), "short read on fuse device") {
		t.Fatalf("stderr = %q, want cause text", errBuf.String())
	}
}
