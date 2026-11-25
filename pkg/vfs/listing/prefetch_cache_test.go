package listing

import (
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
)

type freshnessOnlyHost struct {
	stubHost
	projectionCalls int
}

func (h *freshnessOnlyHost) FreshListCache(string, time.Time) ([]drive.Entry, bool) {
	h.projectionCalls++
	return nil, true
}

func TestPrefetchFreshnessDoesNotProjectDirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cache   map[string]listCacheEntry
		isFresh bool
	}{
		{name: "missing"},
		{name: "expired", cache: map[string]listCacheEntry{"/dir": {expires: time.Now().Add(-time.Minute)}}},
		{name: "fresh", cache: map[string]listCacheEntry{"/dir": {expires: time.Now().Add(time.Minute)}}, isFresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := &freshnessOnlyHost{stubHost: stubHost{cache: tc.cache}}
			lister := NewLister(ListerDeps{Remote: host, View: host, State: NewState()})
			if got := lister.HasFreshListCache("/dir"); got != tc.isFresh {
				t.Fatalf("fresh = %v, want %v", got, tc.isFresh)
			}
			if host.projectionCalls != 0 {
				t.Fatalf("prefetch freshness projected the directory %d times", host.projectionCalls)
			}
		})
	}
}
