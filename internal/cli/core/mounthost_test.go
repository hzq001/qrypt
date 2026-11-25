package core

import (
	"testing"

	corepkg "github.com/yinzhenyu/qrypt/pkg/core"
	"github.com/yinzhenyu/qrypt/pkg/mount"
)

func TestFuseMountSessionImplementsMountSessionState(t *testing.T) {
	t.Parallel()
	var _ corepkg.MountSession = fuseMountSession{}
	var _ corepkg.MountSessionState = fuseMountSession{}

	var empty fuseMountSession
	if empty.IsMounted() {
		t.Fatal("nil session reported IsMounted() == true")
	}
	if empty.Err() != nil {
		t.Fatalf("nil session Err() = %v, want nil", empty.Err())
	}

	mp := t.TempDir()
	adapted := fuseMountSession{session: &mount.Session{ID: mp, MountPoint: mp}}
	if adapted.ID() != mp || adapted.MountPoint() != mp {
		t.Fatalf("ID/MountPoint = (%q, %q), want %q", adapted.ID(), adapted.MountPoint(), mp)
	}
	if adapted.IsMounted() {
		t.Fatalf("unmounted TempDir %q reported IsMounted() == true", mp)
	}
}
