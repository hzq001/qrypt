//go:build darwin

package mount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFuseTHelperAliveMatchesMountWithSpaces(t *testing.T) {
	mountPoint := filepath.Join(t.TempDir(), "Qrypt FSKit")
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(mountPoint)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '/usr/local/bin/go-nfsv4 --backend fskit " + resolved + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	if !fuseTHelperAlive(mountPoint) {
		t.Fatal("live helper for a mount containing spaces was not recognised")
	}
	if fuseTHelperAlive(mountPoint + "-other") {
		t.Fatal("helper for a different mount was reported alive")
	}
}

func TestWaitForFuseTHelperExit(t *testing.T) {
	previous := fuseTHelperAliveProbe
	t.Cleanup(func() { fuseTHelperAliveProbe = previous })
	probes := 0
	fuseTHelperAliveProbe = func(string) bool {
		probes++
		return probes < 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForFuseTHelperExit(ctx, testMountPoint); err != nil {
		t.Fatal(err)
	}
	if probes != 3 {
		t.Fatalf("helper probes = %d, want to wait until it exits", probes)
	}
}

func TestWaitForFuseTHelperExitHonorsContext(t *testing.T) {
	previous := fuseTHelperAliveProbe
	t.Cleanup(func() { fuseTHelperAliveProbe = previous })
	fuseTHelperAliveProbe = func(string) bool { return true }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := waitForFuseTHelperExit(ctx, testMountPoint); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForFuseTHelperExit = %v, want deadline exceeded", err)
	}
}
