package mount

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/winfsp/cgofuse/fuse"
	"github.com/yinzhenyu/qrypt/pkg/drive"
)

type stalledMetadataFS struct {
	stubFS
	stall    bool
	returned bool
}

type lateMetadataFS struct{ stubFS }

func (lateMetadataFS) Stat(context.Context, string) (drive.Entry, error) {
	// Deliberately ignore cancellation: the adapter must wait for the call,
	// rather than replying while the underlying operation is still running.
	time.Sleep(11 * time.Second)
	return drive.Entry{ID: "dir", Name: "dir", IsDir: true}, nil
}

type slowWriteFS struct{ stubFS }

func (slowWriteFS) WriteAt(ctx context.Context, _ string, data []byte, _ int64) (int, error) {
	select {
	case <-time.After(35 * time.Second):
		return len(data), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (s *stalledMetadataFS) Stat(ctx context.Context, path string) (drive.Entry, error) {
	if !s.stall {
		return s.stubFS.Stat(ctx, path)
	}
	<-ctx.Done()
	s.returned = true
	return drive.Entry{}, ctx.Err()
}

func (s *stalledMetadataFS) List(ctx context.Context, path string) ([]drive.Entry, error) {
	if !s.stall {
		return s.stubFS.List(ctx, path)
	}
	<-ctx.Done()
	s.returned = true
	return nil, ctx.Err()
}

func TestMetadataCallbacksBoundRemoteStalls(t *testing.T) {
	t.Parallel()
	callbacks := map[string]func(*adapter) int{
		"Getattr": func(a *adapter) int { return a.Getattr("/dir", &fuse.Stat_t{}, 0) },
		"Access":  func(a *adapter) int { return a.Access("/dir", fuse.R_OK) },
		"Opendir": func(a *adapter) int {
			errc, fh := a.Opendir("/dir")
			if errc == 0 {
				a.Releasedir("/dir", fh)
			}
			return errc
		},
		"Readdir": func(a *adapter) int {
			return a.Readdir("/dir", func(string, *fuse.Stat_t, int64) bool { return true }, 0, 0)
		},
	}
	for name, callback := range callbacks {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fs := &stalledMetadataFS{
					stubFS: stubFS{
						entries: map[string]drive.Entry{"/dir": {ID: "dir", Name: "dir", IsDir: true}},
						lists:   map[string][]drive.Entry{"/dir": {{ID: "file", Name: "file.txt"}}},
					},
					stall: true,
				}
				a := newAdapter(fs, StatfsOptions{})
				defer a.shutdown()
				start := time.Now()
				if got := callback(a); got != -fuse.EIO {
					t.Fatalf("stalled callback = %d, want EIO", got)
				}
				if elapsed := time.Since(start); elapsed != 10*time.Second {
					t.Fatalf("callback duration = %s, want 10s", elapsed)
				}
				if !fs.returned {
					t.Fatal("callback returned before the filesystem acknowledged cancellation")
				}
				if ops := a.activeOpsSnapshot(); len(ops) != 0 {
					t.Fatalf("timed-out operation still active: %+v", ops)
				}
				fs.stall = false
				if got := callback(a); got != 0 {
					t.Fatalf("callback after remote recovery = %d, want success", got)
				}
			})
		})
	}
}

func TestMetadataCallbackKeepsEarlierDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fs := &stalledMetadataFS{stall: true}
		a := newAdapter(fs, StatfsOptions{})
		defer a.shutdown()
		ctx, cancel := context.WithTimeout(a.ctx, time.Second)
		defer cancel()
		a.ctx = ctx
		start := time.Now()
		if got := a.Getattr("/dir", &fuse.Stat_t{}, 0); got != -fuse.EIO {
			t.Fatalf("Getattr = %d, want EIO", got)
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("callback duration = %s, want 1s", elapsed)
		}
	})
}

func TestMetadataCallbackWaitsForFilesystemReturn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := newAdapter(lateMetadataFS{}, StatfsOptions{})
		defer a.shutdown()
		start := time.Now()
		if got := a.Getattr("/dir", &fuse.Stat_t{}, 0); got != 0 {
			t.Fatalf("Getattr = %d, want completed result", got)
		}
		if elapsed := time.Since(start); elapsed != 11*time.Second {
			t.Fatalf("callback returned before filesystem completion: %s", elapsed)
		}
	})
}

func TestMetadataDeadlineDoesNotLimitWrites(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := newAdapter(slowWriteFS{}, StatfsOptions{})
		defer a.shutdown()
		start := time.Now()
		if got := a.Write("/file.txt", []byte("abc"), 0, 0); got != 3 {
			t.Fatalf("Write = %d, want 3", got)
		}
		if elapsed := time.Since(start); elapsed != 35*time.Second {
			t.Fatalf("write duration = %s, want 35s", elapsed)
		}
	})
}

func TestMetadataCallbackStopsOnShutdown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := newAdapter(&stalledMetadataFS{stall: true}, StatfsOptions{})
		defer a.shutdown()
		result := make(chan int, 1)
		go func() { result <- a.Getattr("/dir", &fuse.Stat_t{}, 0) }()
		synctest.Wait()
		a.shutdown()
		if got := <-result; got != -fuse.EIO {
			t.Fatalf("Getattr during shutdown = %d, want EIO", got)
		}
		if got := a.Getattr("/dir", &fuse.Stat_t{}, 0); got != -fuse.EIO {
			t.Fatalf("Getattr after shutdown = %d, want EIO", got)
		}
	})
}
