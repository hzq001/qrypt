package vfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/vfs/listing"
)

func TestStatFreshParentAbsenceDoesNotHidePendingFile(t *testing.T) {
	t.Parallel()
	fs := newPendingViewVFS(t)
	ctx := listing.WithoutDirPrefetch(context.Background())
	if _, err := fs.List(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(ctx, "/.git"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("initial Stat = %v, want ErrNotFound", err)
	}
	if err := fs.Create(ctx, "/.git"); err != nil {
		t.Fatal(err)
	}
	content := []byte("pending content")
	if _, err := fs.WriteAt(ctx, "/.git", content, 0); err != nil {
		t.Fatal(err)
	}
	if entry, err := fs.Stat(ctx, "/.git"); err != nil || entry.Name != ".git" || entry.Size != int64(len(content)) {
		t.Fatalf("pending Stat = %+v, %v", entry, err)
	}
	if !fs.lister.HasFreshListCache("/") {
		t.Fatal("test must exercise the original fresh parent listing")
	}
}

func TestStatFreshParentAbsenceDoesNotHideNewDirectory(t *testing.T) {
	t.Parallel()
	fs := newPendingViewVFS(t)
	ctx := listing.WithoutDirPrefetch(context.Background())
	if _, err := fs.List(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(ctx, "/.git"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("initial Stat = %v, want ErrNotFound", err)
	}
	created, err := fs.Mkdir(ctx, "/.git")
	if err != nil {
		t.Fatal(err)
	}
	if entry, err := fs.Stat(ctx, "/.git"); err != nil || entry.ID != created.ID || !entry.IsDir {
		t.Fatalf("new directory Stat = %+v, %v", entry, err)
	}
}

func TestStatFreshParentAbsenceRefreshFindsRemoteChild(t *testing.T) {
	t.Parallel()
	drv := drive.NewFakeDriver()
	fs, err := New(drv, Options{StorageDir: t.TempDir(), UploadDelay: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close(context.Background()) })
	ctx := listing.WithoutDirPrefetch(context.Background())
	if _, err := fs.List(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(ctx, "/.git"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("initial Stat = %v, want ErrNotFound", err)
	}
	if err := drv.Seed(map[string]string{".git": "remote child"}); err != nil {
		t.Fatal(err)
	}
	fs.RefreshPath("/")
	if entry, err := fs.Stat(ctx, "/.git"); err != nil || entry.Name != ".git" || entry.Size != int64(len("remote child")) {
		t.Fatalf("Stat after refresh = %+v, %v", entry, err)
	}
}
