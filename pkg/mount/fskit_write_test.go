package mount

import (
	"context"
	"errors"
	"testing"

	"github.com/winfsp/cgofuse/fuse"
)

type writeCommitFS struct {
	createRouteFS
	writeErr error
	flushErr error
}

func TestMountUsesFSKit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		goos    string
		options []string
		want    bool
	}{
		{name: "default", goos: "darwin"},
		{name: "nfs", goos: "darwin", options: []string{"backend=nfs"}},
		{name: "smb", goos: "darwin", options: []string{"backend=smb"}},
		{name: "fskit", goos: "darwin", options: []string{"backend=fskit"}, want: true},
		{name: "combined options", goos: "darwin", options: []string{"noappledouble,backend=fskit"}, want: true},
		{name: "last backend wins", goos: "darwin", options: []string{"backend=fskit", "backend=nfs"}},
		{name: "linux", goos: "linux", options: []string{"backend=fskit"}},
		{name: "windows", goos: "windows", options: []string{"backend=fskit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{PlatformOptions: map[string][]string{tc.goos: tc.options}}
			if got := mountUsesFSKit(opts, tc.goos); got != tc.want {
				t.Fatalf("mountUsesFSKit = %v, want %v", got, tc.want)
			}
		})
	}
}

func (fs *writeCommitFS) WriteAt(ctx context.Context, path string, data []byte, off int64) (int, error) {
	if fs.writeErr != nil {
		return 0, fs.writeErr
	}
	return fs.createRouteFS.WriteAt(ctx, path, data, off)
}

func (fs *writeCommitFS) Flush(ctx context.Context, path string) error {
	if err := fs.createRouteFS.Flush(ctx, path); err != nil {
		return err
	}
	return fs.flushErr
}

func TestFSKitChangesCommitWithoutCloseCallbacks(t *testing.T) {
	t.Parallel()
	changes := []struct {
		name string
		run  func(*adapter) int
	}{
		{name: "create empty file", run: func(a *adapter) int {
			errc, _ := a.Create("/file.txt", 1, fuse.S_IFREG|0o644)
			return errc
		}},
		{name: "mknod empty file", run: func(a *adapter) int {
			return a.Mknod("/file.txt", fuse.S_IFREG|0o644, 0)
		}},
		{name: "write", run: func(a *adapter) int {
			n := a.Write("/file.txt", []byte("data"), 0, 1)
			if n == 4 {
				return 0
			}
			return n
		}},
		{name: "truncate to empty", run: func(a *adapter) int {
			return a.Truncate("/file.txt", 0, 1)
		}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			for _, flushOnWrite := range []bool{false, true} {
				fs := &writeCommitFS{}
				a := newAdapterWithOptions(fs, adapterOptions{FlushOnWrite: flushOnWrite})
				if errc := change.run(a); errc != 0 {
					t.Fatalf("mutation returned %d", errc)
				}
				want := 0
				if flushOnWrite {
					want = 1
				}
				if len(fs.flushes) != want {
					t.Fatalf("flushOnWrite=%v: queued %v, want %d commits without Flush/Release", flushOnWrite, fs.flushes, want)
				}
			}
		})
	}
}

func TestFSKitWriteReportsCommitFailure(t *testing.T) {
	t.Parallel()
	fs := &writeCommitFS{flushErr: errors.New("staging sync failed")}
	a := newAdapterWithOptions(fs, adapterOptions{FlushOnWrite: true})
	if got := a.Write("/file.txt", []byte("data"), 0, 1); got != -fuse.EIO {
		t.Fatalf("Write = %d, want EIO when staging could not be committed", got)
	}
}

func TestFSKitFailedWriteDoesNotCommit(t *testing.T) {
	t.Parallel()
	fs := &writeCommitFS{writeErr: errors.New("write failed")}
	a := newAdapterWithOptions(fs, adapterOptions{FlushOnWrite: true})
	if got := a.Write("/file.txt", []byte("data"), 0, 1); got != -fuse.EIO {
		t.Fatalf("Write = %d, want EIO", got)
	}
	if len(fs.flushes) != 0 {
		t.Fatalf("failed write committed %v", fs.flushes)
	}
}

func TestFSKitReadOnlyWriteDoesNotCommit(t *testing.T) {
	t.Parallel()
	fs := &writeCommitFS{}
	a := newAdapterWithOptions(fs, adapterOptions{FlushOnWrite: true, ReadOnly: true})
	if got := a.Write("/file.txt", []byte("data"), 0, 1); got != -fuse.EROFS {
		t.Fatalf("Write = %d, want EROFS", got)
	}
	if len(fs.flushes) != 0 || len(fs.writes) != 0 {
		t.Fatalf("read-only write reached backend: writes=%v commits=%v", fs.writes, fs.flushes)
	}
}
