package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/vfs"
)

type fakeFileSystem struct{}

func (fakeFileSystem) Stat(context.Context, string) (drive.Entry, error) { return drive.Entry{}, nil }
func (fakeFileSystem) List(context.Context, string) ([]drive.Entry, error) {
	return nil, nil
}
func (fakeFileSystem) Read(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (fakeFileSystem) Create(context.Context, string) error { return nil }
func (fakeFileSystem) WriteAt(context.Context, string, []byte, int64) (int, error) {
	return 0, errors.New("not implemented")
}
func (fakeFileSystem) Flush(context.Context, string) error                { return nil }
func (fakeFileSystem) Mkdir(context.Context, string) (drive.Entry, error) { return drive.Entry{}, nil }
func (fakeFileSystem) Remove(context.Context, string) error               { return nil }
func (fakeFileSystem) RemoveDir(context.Context, string) error            { return nil }
func (fakeFileSystem) Rename(context.Context, string, string) error       { return nil }
func (fakeFileSystem) Truncate(context.Context, string, int64) error      { return nil }

type fakeSession struct {
	id         string
	mountPoint string
	done       chan struct{}
	err        error
	mounted    bool
}

func (s *fakeSession) ID() string            { return s.id }
func (s *fakeSession) MountPoint() string    { return s.mountPoint }
func (s *fakeSession) Done() <-chan struct{} { return s.done }
func (s *fakeSession) Err() error            { return s.err }
func (s *fakeSession) IsMounted() bool       { return s.mounted }
func (s *fakeSession) finish()               { close(s.done) }

type bareSession struct {
	id         string
	mountPoint string
	done       chan struct{}
}

func (s *bareSession) ID() string            { return s.id }
func (s *bareSession) MountPoint() string    { return s.mountPoint }
func (s *bareSession) Done() <-chan struct{} { return s.done }

type fakeMountHost struct {
	mounted   []*fakeSession
	unmounted []string
	mountErr  error
	useBare   bool
	bare      []*bareSession
}

func (h *fakeMountHost) Mount(_ context.Context, _ vfs.FileSystem, req MountRequest) (MountSession, error) {
	if h.mountErr != nil {
		return nil, h.mountErr
	}
	if h.useBare {
		s := &bareSession{id: "id-" + filepath.Base(req.MountPoint), mountPoint: req.MountPoint, done: make(chan struct{})}
		h.bare = append(h.bare, s)
		return s, nil
	}
	s := &fakeSession{id: "id-" + filepath.Base(req.MountPoint), mountPoint: req.MountPoint, done: make(chan struct{}), mounted: true}
	h.mounted = append(h.mounted, s)
	return s, nil
}

func (h *fakeMountHost) Unmount(_ context.Context, session MountSession) error {
	h.unmounted = append(h.unmounted, session.ID())
	switch s := session.(type) {
	case *fakeSession:
		s.mounted = false
		select {
		case <-s.done:
		default:
			s.finish()
		}
	case *bareSession:
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	}
	return nil
}

func newTestManager() *mountManager {
	return newMountManager(&fakeMountHost{})
}

func waitMounts(t *testing.T, m *mountManager, want int) []HostedMount {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := m.list()
		if len(got) == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("mounts = %+v, want %d entries", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMountManagerAttachListDetach(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	fs := fakeFileSystem{}
	mountPoint := filepath.Join(t.TempDir(), "mnt")

	info, err := m.attach(ctx, fs, MountRequest{MountPoint: mountPoint})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if info.ID == "" || info.MountPoint != mountPoint || info.StartedAt.IsZero() {
		t.Fatalf("info = %+v", info)
	}
	got := waitMounts(t, m, 1)
	if got[0].ID != info.ID {
		t.Fatalf("list = %+v, want id %q", got, info.ID)
	}

	if _, err := m.attach(ctx, fs, MountRequest{MountPoint: mountPoint}); err == nil {
		t.Fatal("duplicate mount point was accepted")
	}
	if _, err := m.attach(ctx, fs, MountRequest{MountPoint: "relative"}); err == nil {
		t.Fatal("relative mount point was accepted")
	}

	if err := m.detach(ctx, info.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	waitMounts(t, m, 0)
	if err := m.detach(ctx, info.ID); err == nil {
		t.Fatal("detaching an unknown mount succeeded")
	}
}

func TestMountManagerExternalUnmountRemovesEntry(t *testing.T) {
	host := &fakeMountHost{}
	m := newMountManager(host)
	info, err := m.attach(context.Background(), fakeFileSystem{}, MountRequest{MountPoint: filepath.Join(t.TempDir(), "a")})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitMounts(t, m, 1)
	host.mounted[0].finish()
	waitMounts(t, m, 0)
	if info.ID == "" {
		t.Fatal("empty id")
	}
}

func TestMountManagerCloseAll(t *testing.T) {
	host := &fakeMountHost{}
	m := newMountManager(host)
	ctx := context.Background()
	base := t.TempDir()
	if _, err := m.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: filepath.Join(base, "a")}); err != nil {
		t.Fatalf("attach a: %v", err)
	}
	if _, err := m.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: filepath.Join(base, "b")}); err != nil {
		t.Fatalf("attach b: %v", err)
	}
	m.closeAll(ctx)
	if got := m.list(); len(got) != 0 {
		t.Fatalf("list after close = %+v", got)
	}
	if len(host.unmounted) != 2 {
		t.Fatalf("unmounted = %v, want 2", host.unmounted)
	}
	if _, err := m.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: filepath.Join(base, "c")}); err == nil {
		t.Fatal("attach after close succeeded")
	}
}

func TestCoreBusyCountsHostedMounts(t *testing.T) {
	manager := newMountManager(&fakeMountHost{})
	c := &Core{mounts: manager}
	if c.Busy() {
		t.Fatal("core without mounts or tasks should be idle")
	}
	hosted, err := manager.attach(context.Background(), fakeFileSystem{}, MountRequest{MountPoint: filepath.Join(t.TempDir(), "mnt")})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !c.Busy() {
		t.Fatal("core with a hosted mount must stay busy so it cannot idle-exit")
	}
	if err := manager.detach(context.Background(), hosted.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if c.Busy() {
		t.Fatal("core should be idle again after the mount is gone")
	}
}

func waitEndedOutcome(t *testing.T, c *Core, id string) EndedMount {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if outcome, ok := c.MountOutcome(id); ok {
			return outcome
		}
		if time.Now().After(deadline) {
			t.Fatalf("MountOutcome(%q) stayed missing", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMountManagerOutcomesAndControlService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("external unmount", func(t *testing.T) {
		t.Parallel()
		host := &fakeMountHost{}
		c := &Core{mounts: newMountManager(host)}
		svc := coreMountService{core: c}
		mountPoint := filepath.Join(t.TempDir(), "ext")
		info, err := c.mounts.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: mountPoint})
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		activeOutcome, err := svc.MountOutcome(ctx, info.ID)
		if err != nil || !activeOutcome.Active {
			t.Fatalf("active MountOutcome = (%+v, %v), want active", activeOutcome, err)
		}
		if _, ok := c.MountOutcome(info.ID); ok {
			t.Fatal("Core.MountOutcome reported an active mount as ended")
		}

		host.mounted[0].mounted = false
		host.mounted[0].err = errors.New("fuse stopped")
		host.mounted[0].finish()
		waitMounts(t, c.mounts, 0)

		outcome := waitEndedOutcome(t, c, info.ID)
		if outcome.Reason != MountEndExternal || outcome.Error != "" || outcome.MountPoint != mountPoint {
			t.Fatalf("external outcome = %+v", outcome)
		}
		ctrlOutcome, err := svc.MountOutcome(ctx, info.ID)
		if err != nil || ctrlOutcome.Active || ctrlOutcome.Reason != "external" || ctrlOutcome.Error != "" {
			t.Fatalf("control outcome = (%+v, %v)", ctrlOutcome, err)
		}
	})

	t.Run("unexpected end while mounted", func(t *testing.T) {
		t.Parallel()
		host := &fakeMountHost{}
		c := &Core{mounts: newMountManager(host)}
		svc := coreMountService{core: c}
		mountPoint := filepath.Join(t.TempDir(), "dead")
		info, err := c.mounts.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: mountPoint})
		if err != nil {
			t.Fatalf("attach: %v", err)
		}

		host.mounted[0].mounted = true
		host.mounted[0].err = errors.New("short read on fuse device")
		host.mounted[0].finish()
		waitMounts(t, c.mounts, 0)

		outcome := waitEndedOutcome(t, c, info.ID)
		if outcome.Reason != MountEndUnexpected || outcome.Error != "short read on fuse device" {
			t.Fatalf("unexpected outcome = %+v", outcome)
		}
		ctrlOutcome, err := svc.MountOutcome(ctx, info.ID)
		if err != nil || ctrlOutcome.Active || ctrlOutcome.Reason != "unexpected" || ctrlOutcome.Error != "short read on fuse device" {
			t.Fatalf("control outcome = (%+v, %v)", ctrlOutcome, err)
		}
	})

	t.Run("unexpected fallback without MountSessionState", func(t *testing.T) {
		t.Parallel()
		host := &fakeMountHost{useBare: true}
		c := &Core{mounts: newMountManager(host)}
		mountPoint := filepath.Join(t.TempDir(), "bare")
		info, err := c.mounts.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: mountPoint})
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		close(host.bare[0].done)
		waitMounts(t, c.mounts, 0)

		outcome := waitEndedOutcome(t, c, info.ID)
		if outcome.Reason != MountEndUnexpected || outcome.Error != "FUSE session ended unexpectedly" {
			t.Fatalf("bare session outcome = %+v", outcome)
		}
	})

	t.Run("detach records detached before unmount and re-attach clears outcome", func(t *testing.T) {
		t.Parallel()
		host := &fakeMountHost{}
		c := &Core{mounts: newMountManager(host)}
		svc := coreMountService{core: c}
		mountPoint := filepath.Join(t.TempDir(), "det")
		info, err := c.mounts.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: mountPoint})
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		if err := c.mounts.detach(ctx, info.ID); err != nil {
			t.Fatalf("detach: %v", err)
		}
		// Give watch goroutine a chance to run; detached must remain intact.
		time.Sleep(20 * time.Millisecond)
		outcome := waitEndedOutcome(t, c, info.ID)
		if outcome.Reason != MountEndDetached || outcome.Error != "" {
			t.Fatalf("detached outcome = %+v", outcome)
		}
		ctrlOutcome, err := svc.MountOutcome(ctx, info.ID)
		if err != nil || ctrlOutcome.Active || ctrlOutcome.Reason != "detached" {
			t.Fatalf("control outcome = (%+v, %v)", ctrlOutcome, err)
		}

		// Re-attaching the same mount point (same ID) clears the prior ended outcome.
		reattached, err := c.mounts.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: mountPoint})
		if err != nil {
			t.Fatalf("re-attach: %v", err)
		}
		if _, ok := c.MountOutcome(reattached.ID); ok {
			t.Fatal("stale ended outcome remained after re-attach")
		}
		activeOutcome, err := svc.MountOutcome(ctx, reattached.ID)
		if err != nil || !activeOutcome.Active {
			t.Fatalf("re-attached MountOutcome = (%+v, %v), want active", activeOutcome, err)
		}
		if err := c.mounts.detach(ctx, reattached.ID); err != nil {
			t.Fatalf("detach reattached: %v", err)
		}
	})

	t.Run("closeAll records detached", func(t *testing.T) {
		t.Parallel()
		host := &fakeMountHost{}
		c := &Core{mounts: newMountManager(host)}
		mountPoint := filepath.Join(t.TempDir(), "close")
		info, err := c.mounts.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: mountPoint})
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		c.mounts.closeAll(ctx)
		outcome := waitEndedOutcome(t, c, info.ID)
		if outcome.Reason != MountEndDetached {
			t.Fatalf("closeAll outcome = %+v, want detached", outcome)
		}
	})

	t.Run("ended map stays bounded", func(t *testing.T) {
		t.Parallel()
		host := &fakeMountHost{}
		c := &Core{mounts: newMountManager(host)}
		base := t.TempDir()
		var ids []string
		for i := range maxEndedMounts + 5 {
			mp := filepath.Join(base, fmt.Sprintf("m-%03d", i))
			info, err := c.mounts.attach(ctx, fakeFileSystem{}, MountRequest{MountPoint: mp})
			if err != nil {
				t.Fatalf("attach %d: %v", i, err)
			}
			ids = append(ids, info.ID)
			if err := c.mounts.detach(ctx, info.ID); err != nil {
				t.Fatalf("detach %d: %v", i, err)
			}
		}
		for i := range 5 {
			if _, ok := c.MountOutcome(ids[i]); ok {
				t.Fatalf("evicted id %q still present", ids[i])
			}
		}
		for i := 5; i < len(ids); i++ {
			if _, ok := c.MountOutcome(ids[i]); !ok {
				t.Fatalf("recent id %q missing", ids[i])
			}
		}
	})
}
