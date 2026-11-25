package vfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/drivers/localfs"
)

// countingListDriver counts backend listings of the VFS root while delegating
// to a real local filesystem driver. Child-directory prefetches are
// asynchronous, so counting every List call would make the assertions racy;
// only the listing under test is counted.
type countingListDriver struct {
	drive.Driver
	rootID string
	lists  *atomic.Int64
}

func (d *countingListDriver) List(ctx context.Context, parentID string) ([]drive.Entry, error) {
	if d.lists != nil && parentID == d.rootID {
		d.lists.Add(1)
	}
	return d.Driver.List(ctx, parentID)
}

func newListCacheTestVFS(t *testing.T, root, cacheDir, identity string, lists *atomic.Int64) *VFS {
	t.Helper()
	fs, err := New(&countingListDriver{Driver: localfs.New(root), rootID: "0", lists: lists}, Options{
		Name:              "m",
		StorageDir:        t.TempDir(),
		ListCacheDir:      cacheDir,
		ListCacheTTL:      time.Minute,
		ListCacheMaxBytes: 1 << 20,
		ListCacheIdentity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestVFSListCacheServesSecondListingAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "m")
	var lists atomic.Int64
	fs := newListCacheTestVFS(t, root, cacheDir, "identity-1", &lists)
	ctx := context.Background()

	first, err := fs.List(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if got := lists.Load(); got != 1 {
		t.Fatalf("driver lists = %d, want 1", got)
	}
	second, err := fs.List(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if got := lists.Load(); got != 1 {
		t.Fatalf("driver lists after cached list = %d, want 1", got)
	}
	if len(first) != len(second) {
		t.Fatalf("cached listing length = %d, want %d", len(second), len(first))
	}

	// A local mutation drops the persisted listing, so the next list refetches.
	if _, err := fs.Mkdir(ctx, "/newdir"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.List(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if got := lists.Load(); got != 2 {
		t.Fatalf("driver lists after mutation = %d, want 2", got)
	}

	// A second VFS with the same cache directory (a restart) serves the
	// persisted listing without touching the backend.
	var restartLists atomic.Int64
	fs2 := newListCacheTestVFS(t, root, cacheDir, "identity-1", &restartLists)
	if _, err := fs2.List(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if got := restartLists.Load(); got != 0 {
		t.Fatalf("restart driver lists = %d, want 0", got)
	}
	// A different identity must not serve the previous account's listing.
	var foreignLists atomic.Int64
	fs3 := newListCacheTestVFS(t, root, cacheDir, "identity-2", &foreignLists)
	if _, err := fs3.List(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if got := foreignLists.Load(); got != 1 {
		t.Fatalf("foreign identity driver lists = %d, want 1", got)
	}
	for _, f := range []*VFS{fs, fs2, fs3} {
		if err := f.CloseReadCache(); err != nil {
			t.Fatal(err)
		}
	}
}

type extraRequiringDriver struct {
	drive.Driver
	rootID string
	lists  *atomic.Int64
}

func (d *extraRequiringDriver) List(ctx context.Context, parentID string) ([]drive.Entry, error) {
	if d.lists != nil && parentID == d.rootID {
		d.lists.Add(1)
	}
	entries, err := d.Driver.List(ctx, parentID)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].IsDir {
			continue
		}
		entries[i].Extra = map[string]any{
			"fs_id": "42",
			"md5":   "abc",
		}
	}
	return entries, nil
}

func (d *extraRequiringDriver) Read(ctx context.Context, entry drive.Entry, offset, size int64) (io.ReadCloser, error) {
	extra, ok := drive.EntryRawExtra(entry).(map[string]any)
	if !ok || extra["fs_id"] != "42" {
		return nil, fmt.Errorf("missing fs_id for %q (extra=%#v)", entry.ID, entry.Extra)
	}
	return d.Driver.Read(ctx, entry, offset, size)
}

func TestVFSListCachePreservesEntryExtraAcrossRestart(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "m")
	ctx := context.Background()

	var firstLists atomic.Int64
	fs1, err := New(&extraRequiringDriver{Driver: localfs.New(root), rootID: "0", lists: &firstLists}, Options{
		Name:              "m",
		StorageDir:        t.TempDir(),
		ListCacheDir:      cacheDir,
		ListCacheTTL:      time.Minute,
		ListCacheMaxBytes: 1 << 20,
		ListCacheIdentity: "identity-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fs1.CloseReadCache(); err != nil {
			t.Errorf("fs1.CloseReadCache: %v", err)
		}
	})

	if _, err := fs1.List(ctx, "/"); err != nil {
		t.Fatalf("fs1.List: %v", err)
	}
	if firstLists.Load() != 1 {
		t.Fatalf("fs1 lists = %d, want 1", firstLists.Load())
	}

	var restartLists atomic.Int64
	fs2, err := New(&extraRequiringDriver{Driver: localfs.New(root), rootID: "0", lists: &restartLists}, Options{
		Name:              "m",
		StorageDir:        t.TempDir(),
		ListCacheDir:      cacheDir,
		ListCacheTTL:      time.Minute,
		ListCacheMaxBytes: 1 << 20,
		ListCacheIdentity: "identity-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fs2.CloseReadCache(); err != nil {
			t.Errorf("fs2.CloseReadCache: %v", err)
		}
	})

	entries, err := fs2.List(ctx, "/")
	if err != nil {
		t.Fatalf("fs2.List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("fs2 entries = %+v", entries)
	}
	if restartLists.Load() != 0 {
		t.Fatalf("restart lists = %d, want 0", restartLists.Load())
	}

	rc, err := fs2.Read(ctx, "/a.txt", 0, 1)
	if err != nil {
		t.Fatalf("fs2.Read: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if string(data) != "a" {
		t.Fatalf("data = %q, want %q", string(data), "a")
	}
	if restartLists.Load() != 0 {
		t.Fatalf("restart lists after Read = %d, want 0", restartLists.Load())
	}
}
