package listcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "lists"), "m1", "id-1", time.Minute, 1<<20)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if store == nil {
		t.Fatal("store disabled")
	}
	return store
}

func entries() []drive.Entry {
	return []drive.Entry{{ID: "1", Name: "a.txt", Size: 1}, {ID: "2", Name: "sub", IsDir: true}}
}

func TestStoreHitAndMiss(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	if _, ok := store.Get("/dir", now); ok {
		t.Fatal("empty store hit")
	}
	store.Put("/dir", entries(), 7, now)
	got, ok := store.Get("/dir", now.Add(time.Second))
	if !ok || len(got) != 2 || got[1].Name != "sub" {
		t.Fatalf("hit = %v %v", got, ok)
	}
	if _, ok := store.Get("/other", now); ok {
		t.Fatal("unknown path hit")
	}
	if stats := store.Stats(); stats.Hits != 1 || stats.Puts != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestStoreExpiresAndDropsStaleGeneration(t *testing.T) {
	store := newTestStore(t)
	store.ttl = 10 * time.Second
	now := time.Now()
	store.Put("/dir", entries(), 1, now)
	if _, ok := store.Get("/dir", now.Add(11*time.Second)); ok {
		t.Fatal("expired listing served")
	}
	if _, ok := store.Get("/dir", now); ok {
		t.Fatal("expired file not removed")
	}
}

func TestStoreIdentityMismatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lists")
	if _, err := NewStore(dir, "m1", "old-account", time.Minute, 1<<20); err != nil {
		t.Fatal(err)
	}
	first, _ := NewStore(dir, "m1", "old-account", time.Minute, 1<<20)
	first.Put("/dir", entries(), 1, time.Now())
	second, _ := NewStore(dir, "m1", "new-account", time.Minute, 1<<20)
	if _, ok := second.Get("/dir", time.Now()); ok {
		t.Fatal("served listing from another identity")
	}
}

func TestStoreCorruptFileIsDropped(t *testing.T) {
	store := newTestStore(t)
	path := store.filePath("/dir")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("/dir", time.Now()); ok {
		t.Fatal("corrupt file served")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt file kept: %v", err)
	}
	if stats := store.Stats(); stats.CorruptDrops != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestStoreEvictsToLowWatermark(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lists")
	store, err := NewStore(dir, "m1", "id", time.Minute, 4096)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 20; i++ {
		store.Put("/dir-"+string(rune('a'+i)), []drive.Entry{{ID: "x", Name: "file.txt", Size: 1024}}, 1, now.Add(time.Duration(i)*time.Millisecond))
	}
	if stats := store.Stats(); stats.Bytes > 4096 {
		t.Fatalf("over cap: %+v", stats)
	}
	if stats := store.Stats(); stats.Evictions == 0 {
		t.Fatalf("no eviction: %+v", stats)
	}
}

func TestStoreClearAndDisabled(t *testing.T) {
	store := newTestStore(t)
	store.Put("/dir", entries(), 1, time.Now())
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("/dir", time.Now()); ok {
		t.Fatal("clear left a listing")
	}
	disabled, err := NewStore("", "m", "id", time.Minute, 0)
	if err != nil || disabled != nil {
		t.Fatalf("disabled store = %v %v", disabled, err)
	}
	var nilStore *Store
	nilStore.Put("/x", entries(), 1, time.Now())
	if _, ok := nilStore.Get("/x", time.Now()); ok {
		t.Fatal("nil store hit")
	}
	if nilStore.Enabled() {
		t.Fatal("nil store enabled")
	}
}

func TestStorePreservesExtraAndRemoteName(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	input := []drive.Entry{
		{
			ID:    "1",
			Name:  "plain-extra.bin",
			Size:  10,
			Extra: map[string]any{"fs_id": "12345", "md5": "deadbeef"},
		},
		drive.WrapRemoteName(drive.Entry{
			ID:    "2",
			Name:  "secret.txt",
			Size:  20,
			Extra: map[string]any{"fs_id": "67890", "md5": "cafe"},
		}, "enc-secret.txt"),
		drive.WrapRemoteName(drive.Entry{
			ID:   "3",
			Name: "plain.txt",
			Size: 30,
		}, "enc-plain.txt"),
		{
			ID:   "4",
			Name: "mixed.bin",
			Size: 40,
			Extra: map[string]any{
				"fs_id":          "999",
				"unserializable": func() {},
			},
		},
	}

	store.Put("/dir", input, 1, now)
	got, ok := store.Get("/dir", now.Add(time.Second))
	if !ok {
		t.Fatal("store.Get missed after Put")
	}
	if len(got) != len(input) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(input))
	}

	raw0, ok := drive.EntryRawExtra(got[0]).(map[string]any)
	if !ok || raw0["fs_id"] != "12345" || raw0["md5"] != "deadbeef" {
		t.Fatalf("entry 0 extra = %#v, want fs_id=12345 md5=deadbeef", drive.EntryRawExtra(got[0]))
	}

	remote1, ok := drive.EntryRemoteName(got[1])
	if !ok || remote1 != "enc-secret.txt" {
		t.Fatalf("entry 1 remote = %q (%v), want enc-secret.txt", remote1, ok)
	}
	raw1, ok := drive.EntryRawExtra(got[1]).(map[string]any)
	if !ok || raw1["fs_id"] != "67890" || raw1["md5"] != "cafe" {
		t.Fatalf("entry 1 raw extra = %#v, want fs_id=67890 md5=cafe", drive.EntryRawExtra(got[1]))
	}

	remote2, ok := drive.EntryRemoteName(got[2])
	if !ok || remote2 != "enc-plain.txt" {
		t.Fatalf("entry 2 remote = %q (%v), want enc-plain.txt", remote2, ok)
	}
	wrapper2, ok := got[2].Extra.(drive.EntryExtraWrapper)
	if !ok || wrapper2.Raw != nil {
		t.Fatalf("entry 2 wrapper = %#v, want nil inner Raw", got[2].Extra)
	}

	raw3, ok := drive.EntryRawExtra(got[3]).(map[string]any)
	if !ok || raw3["fs_id"] != "999" {
		t.Fatalf("entry 3 extra = %#v, want fs_id=999", drive.EntryRawExtra(got[3]))
	}
	if _, exists := raw3["unserializable"]; exists {
		t.Fatalf("entry 3 unexpectedly retained unserializable key: %#v", raw3)
	}
}

func TestStoreDropsLegacySchemaVersion(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	legacyJSON := `{"schema":1,"mount":"m1","path":"/dir","identity":"id-1","generation":1,"fetched_at":"` + now + `","entries":[{"id":"1","name":"a.txt","is_dir":false,"size":1}]}`
	path := store.filePath("/dir")
	if err := os.WriteFile(path, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("/dir", time.Now()); ok {
		t.Fatal("legacy schema v1 file served")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy schema v1 file kept: %v", err)
	}
	if stats := store.Stats(); stats.CorruptDrops != 1 {
		t.Fatalf("CorruptDrops = %d, want 1", stats.CorruptDrops)
	}
}
