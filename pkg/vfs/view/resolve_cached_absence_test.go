package view

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
)

func TestResolveMissingChildUsesFreshParentListing(t *testing.T) {
	t.Parallel()
	v, overlay, _ := newTestDomain(t)
	NewRuntime(v).CommitChildren("/dir", []drive.Entry{{ID: "present", Name: "present.txt"}}, time.Now().Add(time.Minute))
	parentCalls, listCalls := 0, 0
	_, err := ResolveWithRuntime(context.Background(), "/dir/.git", NewResolve(v, overlay),
		func(context.Context, string) (drive.Entry, error) {
			parentCalls++
			return drive.Entry{ID: "dir", IsDir: true}, nil
		},
		func(context.Context, string, string) ([]drive.Entry, error) {
			listCalls++
			return nil, nil
		},
	)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve error = %v, want ErrNotFound", err)
	}
	if parentCalls != 0 || listCalls != 0 {
		t.Fatalf("fresh absence repeated parent resolution/list projection: parent=%d list=%d", parentCalls, listCalls)
	}
}

func TestResolveCachedAbsenceRespectsExpiryInvalidationAndLocalEntries(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"expired", "invalidated", "listed child evicted", "local child"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			v, overlay, _ := newTestDomain(t)
			runtime := NewRuntime(v)
			child := drive.Entry{ID: "git", Name: ".git", IsDir: true}
			runtime.CommitChildren("/dir", nil, time.Now().Add(time.Minute))
			switch scenario {
			case "expired":
				runtime.CommitChildren("/dir", nil, time.Now().Add(-time.Second))
			case "invalidated":
				runtime.RefreshPath("/dir")
			case "listed child evicted":
				runtime.CommitChildren("/dir", []drive.Entry{child}, time.Now().Add(time.Minute))
				v.entries.Delete("/dir/.git")
			case "local child":
				v.entries.Set("/dir/.git", child)
			}
			listCalls := 0
			got, err := ResolveWithRuntime(context.Background(), "/dir/.git", NewResolve(v, overlay),
				func(context.Context, string) (drive.Entry, error) { return drive.Entry{ID: "dir", IsDir: true}, nil },
				func(context.Context, string, string) ([]drive.Entry, error) {
					listCalls++
					return []drive.Entry{child}, nil
				},
			)
			if err != nil || got.ID != child.ID {
				t.Fatalf("resolve = %+v, %v", got, err)
			}
			wantCalls := 1
			if scenario == "local child" {
				wantCalls = 0
			}
			if listCalls != wantCalls {
				t.Fatalf("list calls = %d, want %d", listCalls, wantCalls)
			}
		})
	}
}

type commitAfterCacheMissResolver struct {
	Resolve
	commit func()
}

func (r commitAfterCacheMissResolver) CachedEntry(path string) (drive.Entry, bool) {
	entry, ok := r.Resolve.CachedEntry(path)
	if !ok {
		r.commit()
	}
	return entry, ok
}

func TestResolveCachedAbsenceSeesChildCommittedAfterCacheMiss(t *testing.T) {
	t.Parallel()
	v, overlay, _ := newTestDomain(t)
	runtime := NewRuntime(v)
	runtime.CommitChildren("/dir", nil, time.Now().Add(time.Minute))
	child := drive.Entry{ID: "git", Name: ".git", IsDir: true}
	resolver := commitAfterCacheMissResolver{
		Resolve: NewResolve(v, overlay),
		commit: func() {
			// Cache warming can add a child without invalidating the fresh
			// parent listing, between the two resolution checks.
			runtime.CacheListedChildren("/dir", []drive.Entry{child})
		},
	}
	got, err := ResolveWithRuntime(context.Background(), "/dir/.git", resolver,
		func(context.Context, string) (drive.Entry, error) {
			return drive.Entry{ID: "dir", IsDir: true}, nil
		},
		func(context.Context, string, string) ([]drive.Entry, error) {
			entry, ok := runtime.CachedEntry("/dir/.git")
			if !ok {
				t.Fatal("concurrently committed child is missing")
			}
			return []drive.Entry{entry}, nil
		},
	)
	if err != nil || got.ID != child.ID {
		t.Fatalf("resolve after local commit = %+v, %v", got, err)
	}
}
