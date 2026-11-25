package listing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
)

type delayedListHost struct {
	mu sync.RWMutex
	fakeRuntimeHost
	calls int
}

func (h *delayedListHost) ListChildren(context.Context, string) ([]drive.Entry, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	time.Sleep(2 * listCacheTTL)
	return h.children, nil
}

func (h *delayedListHost) HasFreshListCache(parentPath string, now time.Time) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.fakeRuntimeHost.HasFreshListCache(parentPath, now)
}

func (h *delayedListHost) FreshListCache(parentPath string, now time.Time) ([]drive.Entry, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.fakeRuntimeHost.FreshListCache(parentPath, now)
}

func (h *delayedListHost) CommitRemoteChildren(parentPath string, entries []drive.Entry, expires time.Time) []drive.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cache = map[string]listCacheEntry{parentPath: {entries: entries, expires: expires}}
	return h.fakeRuntimeHost.CommitRemoteChildren(parentPath, entries, expires)
}

func (h *delayedListHost) callCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.calls
}

func TestSlowListingCacheLifetimeStartsAfterRemoteCompletes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		host := &delayedListHost{fakeRuntimeHost: fakeRuntimeHost{
			children: []drive.Entry{{ID: "child", Name: "child.txt"}},
		}}
		lister := NewLister(ListerDeps{Remote: host, View: host, State: NewState()})
		for range 2 {
			entries, err := lister.Children(context.Background(), "/dir", "parent")
			if err != nil || len(entries) != 1 {
				t.Fatalf("Children = %+v, %v", entries, err)
			}
		}
		if calls := host.callCount(); calls != 1 {
			t.Fatalf("backend calls immediately after slow listing = %d, want 1", calls)
		}
		time.Sleep(listCacheTTL - time.Nanosecond)
		if _, err := lister.Children(context.Background(), "/dir", "parent"); err != nil {
			t.Fatal(err)
		}
		if calls := host.callCount(); calls != 1 {
			t.Fatalf("backend calls before cache expiry = %d, want 1", calls)
		}
		time.Sleep(time.Nanosecond)
		if _, err := lister.Children(context.Background(), "/dir", "parent"); err != nil {
			t.Fatal(err)
		}
		if calls := host.callCount(); calls != 2 {
			t.Fatalf("backend calls after cache expiry = %d, want 2", calls)
		}
	})
}

func TestForegroundDeadlineDoesNotCancelSharedPrefetch(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		host := &delayedListHost{fakeRuntimeHost: fakeRuntimeHost{
			children: []drive.Entry{{ID: "child", Name: "child.txt"}},
			entry:    drive.Entry{ID: "parent", IsDir: true},
			entryOK:  true,
		}}
		lister := NewLister(ListerDeps{Remote: host, View: host, State: NewState()})
		prefetchDone := make(chan error, 1)
		go func() {
			_, err := lister.prefetchChildren(context.Background(), "/dir", "parent")
			prefetchDone <- err
		}()
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := lister.Children(ctx, "/dir", "parent"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting foreground lookup = %v, want deadline exceeded", err)
		}
		select {
		case err := <-prefetchDone:
			t.Fatalf("foreground timeout ended the shared prefetch: %v", err)
		default:
		}
		if err := <-prefetchDone; err != nil {
			t.Fatalf("shared prefetch failed: %v", err)
		}
		entries, err := lister.Children(context.Background(), "/dir", "parent")
		if err != nil || len(entries) != 1 || host.callCount() != 1 {
			t.Fatalf("lookup after prefetch = %+v, %v, backend calls=%d", entries, err, host.callCount())
		}
	})
}
