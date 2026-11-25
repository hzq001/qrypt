package listing

import (
	"context"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
)

type fakeListCache struct {
	entries    map[string][]drive.Entry
	generation map[string]uint64
	gets       int
	puts       int
	putGen     uint64
}

func (c *fakeListCache) Get(path string, _ time.Time) ([]drive.Entry, bool) {
	c.gets++
	entries, ok := c.entries[path]
	return entries, ok
}

func (c *fakeListCache) Put(path string, entries []drive.Entry, generation uint64, _ time.Time) {
	c.puts++
	c.putGen = generation
	if c.entries == nil {
		c.entries = map[string][]drive.Entry{}
		c.generation = map[string]uint64{}
	}
	c.entries[path] = entries
	c.generation[path] = generation
}

type cacheHost struct {
	*fakeRuntimeHost
	generation uint64
	resolves   int
	lists      int
}

func (h *cacheHost) Resolve(ctx context.Context, path string) (drive.Entry, error) {
	h.resolves++
	return drive.Entry{ID: "dir-id", Name: "dir", IsDir: true}, nil
}

func (h *cacheHost) ListChildren(ctx context.Context, parentID string) ([]drive.Entry, error) {
	h.lists++
	return []drive.Entry{{ID: "a", Name: "a.txt", Size: 1}}, nil
}

func (h *cacheHost) ListGeneration() uint64 { return h.generation }

func (h *cacheHost) ProjectChildren(_ string, entries []drive.Entry) []drive.Entry {
	return entries
}

func (h *cacheHost) CommitRemoteChildren(_ string, remote []drive.Entry, _ time.Time) []drive.Entry {
	return remote
}

func TestListerServesPersistedListingWithoutRemoteCalls(t *testing.T) {
	host := &cacheHost{fakeRuntimeHost: &fakeRuntimeHost{}}
	cache := &fakeListCache{}
	lister := NewLister(ListerDeps{Remote: host, View: host, State: NewState(), ListCache: cache})
	ctx := context.Background()

	entries, err := lister.ListNoPrefetch(ctx, "/dir")
	if err != nil || len(entries) != 1 {
		t.Fatalf("first list = %v, %v", entries, err)
	}
	if host.resolves != 1 || host.lists != 1 {
		t.Fatalf("remote calls = %d/%d, want 1/1", host.resolves, host.lists)
	}
	if cache.puts != 1 || cache.putGen != 0 {
		t.Fatalf("put = %d gen %d", cache.puts, cache.putGen)
	}

	entries, err = lister.ListNoPrefetch(ctx, "/dir")
	if err != nil || len(entries) != 1 {
		t.Fatalf("cached list = %v, %v", entries, err)
	}
	if host.resolves != 1 || host.lists != 1 {
		t.Fatalf("cached remote calls = %d/%d, want 1/1", host.resolves, host.lists)
	}

	// Mutation invalidation is owned by the VFS committer, which deletes the
	// affected persisted listing and advances the generation for in-flight
	// fetches; the lister itself only stores the generation for the Put guard.
	host.generation = 1
	cache.entries = nil
	cache.generation = nil
	if _, err := lister.ListNoPrefetch(ctx, "/dir"); err != nil {
		t.Fatal(err)
	}
	if host.resolves != 2 || host.lists != 2 {
		t.Fatalf("post-invalidation remote calls = %d/%d, want 2/2", host.resolves, host.lists)
	}
	if cache.putGen != 1 {
		t.Fatalf("put generation = %d, want 1", cache.putGen)
	}
}

func TestListerPrefersInMemoryFreshListBeforePersistedCache(t *testing.T) {
	type customExtra struct {
		Tag string
	}
	memEntries := []drive.Entry{{
		ID:    "a",
		Name:  "a.txt",
		Size:  1,
		Extra: customExtra{Tag: "in-memory"},
	}}
	host := &cacheHost{
		fakeRuntimeHost: &fakeRuntimeHost{
			stubHost: stubHost{
				cache: map[string]listCacheEntry{
					"/dir": {
						entries: memEntries,
						expires: time.Now().Add(time.Minute),
					},
				},
			},
		},
	}
	cache := &fakeListCache{
		entries: map[string][]drive.Entry{
			"/dir": {{ID: "a", Name: "a.txt", Size: 1}},
		},
	}
	lister := NewLister(ListerDeps{Remote: host, View: host, State: NewState(), ListCache: cache})
	ctx := context.Background()

	got, err := lister.ListNoPrefetch(ctx, "/dir")
	if err != nil || len(got) != 1 {
		t.Fatalf("ListNoPrefetch = %v, %v", got, err)
	}
	if cache.gets != 0 {
		t.Fatalf("ListNoPrefetch consulted persisted cache (%d gets) while in-memory list was fresh", cache.gets)
	}
	if extra, ok := got[0].Extra.(customExtra); !ok || extra.Tag != "in-memory" {
		t.Fatalf("ListNoPrefetch extra = %#v, want in-memory customExtra", got[0].Extra)
	}

	got, err = lister.Children(ctx, "/dir", "dir-id")
	if err != nil || len(got) != 1 {
		t.Fatalf("Children = %v, %v", got, err)
	}
	if cache.gets != 0 {
		t.Fatalf("Children consulted persisted cache (%d gets) while in-memory list was fresh", cache.gets)
	}
	if extra, ok := got[0].Extra.(customExtra); !ok || extra.Tag != "in-memory" {
		t.Fatalf("Children extra = %#v, want in-memory customExtra", got[0].Extra)
	}

	// Once in-memory cache expires or disappears, fall back to persisted ListCache.
	host.cache = nil
	got, err = lister.ListNoPrefetch(ctx, "/dir")
	if err != nil || len(got) != 1 {
		t.Fatalf("cold ListNoPrefetch = %v, %v", got, err)
	}
	if cache.gets == 0 {
		t.Fatalf("expected persisted cache fallback after in-memory eviction, gets = %d", cache.gets)
	}
}
