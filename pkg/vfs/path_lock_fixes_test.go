package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/drivers/localfs"
)

// TestLifecycleContextConcurrentWithStart exercises the data race between
// Start storing the lifecycle context and background readers (delayed-delete
// timers) loading it; meaningful under -race.
func TestLifecycleContextConcurrentWithStart(t *testing.T) {
	t.Parallel()
	fs, err := New(drive.NewFakeDriver(), Options{StorageDir: t.TempDir(), UploadDelay: time.Hour, DeleteDelay: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = fs.lifecycleContext()
				}
			}
		}()
	}
	ctx, cancel := context.WithCancel(context.Background())
	fs.Start(ctx)
	cancel()
	close(stop)
	wg.Wait()
	if err := fs.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type failingReadRemote struct {
	entry drive.Entry
}

func (r *failingReadRemote) Parent(context.Context, string) (drive.Entry, string, error) {
	return drive.Entry{ID: "parent", Name: "parent", IsDir: true}, "file.txt", nil
}
func (r *failingReadRemote) Resolve(context.Context, string) (drive.Entry, error) {
	return r.entry, nil
}
func (r *failingReadRemote) Read(context.Context, drive.Entry) (io.ReadCloser, error) {
	return nil, errors.New("remote read failed")
}
func (r *failingReadRemote) InvalidateReadCache(drive.Entry) {}

// countStagingFiles counts the mount's staging artifacts, which is what an
// orphan check cares about: the storage directory itself also holds the upload
// and delete journals (pending.jsonl, deletes.jsonl).
func countStagingFiles(t *testing.T, storage string) int {
	t.Helper()
	dir := filepath.Join(storage, "staging")
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return 0
	}
	return countFiles(t, dir)
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestStageExistingCleansStagingOnRemoteError verifies the error paths of
// stageExistingWithDeps drop the staging file instead of leaving orphans.
func TestStageExistingCleansStagingOnRemoteError(t *testing.T) {
	t.Parallel()
	storage := t.TempDir()
	fs, err := New(localfs.New(t.TempDir()), Options{StorageDir: storage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close(context.Background()) })
	remote := &failingReadRemote{entry: drive.Entry{ID: "remote", Name: "file.txt", Size: 5, ModTime: time.Now()}}
	err = fs.stageExistingWithDeps(context.Background(), "/file.txt", fs.uploads, remote)
	if err == nil {
		t.Fatal("expected stageExistingWithDeps to fail")
	}
	if n := countStagingFiles(t, storage); n != 0 {
		t.Fatalf("staging orphan left behind: %d files in staging dir", n)
	}
}
