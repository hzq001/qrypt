package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/vfs"
	idelete "github.com/yinzhenyu/qrypt/pkg/vfs/delete"
)

func TestInspectJournalCacheReportsPendingProblems(t *testing.T) {
	tmp := t.TempDir()
	cacheDir := filepath.Join(tmp, "cache")
	stagingDir := filepath.Join(cacheDir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(stagingDir, "file.staging")
	if err := os.WriteFile(localPath, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(stagingDir, "orphan.staging")
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(cacheDir, "pending.jsonl")
	dirty, err := json.Marshal(struct {
		Op string `json:"op"`
		vfs.PendingUpload
	}{
		Op: "dirty",
		PendingUpload: vfs.PendingUpload{
			Path:       "/file.txt",
			FID:        "file",
			Name:       "file.txt",
			LocalPath:  localPath,
			Size:       4,
			RetryCount: 2,
			LastError:  "upload failed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	clean, err := json.Marshal(struct {
		Op string `json:"op"`
		vfs.PendingUpload
	}{
		Op:            "clean",
		PendingUpload: vfs.PendingUpload{Path: "/old.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := string(dirty) + "\n" + string(clean) + "\n" + "{bad json\n"
	if err := os.WriteFile(journalPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	report := InspectCache(DebugCacheTarget{Name: "test", Dir: cacheDir})
	if report.Entries != 2 || report.DirtyEntries != 1 || report.CleanEntries != 1 {
		t.Fatalf("unexpected journal counts: %+v", report)
	}
	if len(report.InvalidEntries) != 1 {
		t.Fatalf("expected one invalid entry, got %+v", report.InvalidEntries)
	}
	if len(report.Pending) != 1 {
		t.Fatalf("expected one pending entry, got %+v", report.Pending)
	}
	if !report.Pending[0].StagingExists || report.Pending[0].StagingSize != 3 {
		t.Fatalf("expected staging size mismatch details, got %+v", report.Pending[0])
	}
	if len(report.OrphanStaging) != 1 || report.OrphanStaging[0] != orphanPath {
		t.Fatalf("unexpected orphan staging files: %+v", report.OrphanStaging)
	}
}

// TestInspectJournalCacheIncludesDeleteJournal drives the report from a real
// delete journal (written by pkg/vfs/delete itself, so the reader is checked
// against the writer's format) plus a torn tail, the line a crash mid-append
// leaves behind.
func TestInspectJournalCacheIncludesDeleteJournal(t *testing.T) {
	t.Parallel()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(cacheDir, idelete.JournalName)
	if err := os.WriteFile(journalPath, []byte("{torn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := idelete.OpenJournal(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if err := journal.Put(idelete.JournalRecord{
		Path: "/baidu/x.bin", ID: "id-x", Attempts: 1, Error: "baidu api errno 12", UpdatedAt: updated,
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Put(idelete.JournalRecord{Path: "/baidu/gone.bin", ID: "id-gone", Attempts: 1, UpdatedAt: updated}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Done("/baidu/gone.bin"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	report := InspectCache(DebugCacheTarget{Name: "baidu", Dir: cacheDir})
	if report.Deletes.File != idelete.JournalName {
		t.Fatalf("delete report file = %q, want %q so it is distinguishable from pending.jsonl",
			report.Deletes.File, idelete.JournalName)
	}
	if report.Deletes.Entries != 1 || report.Deletes.Failed != 1 {
		t.Fatalf("delete report = %+v, want one failed live record", report.Deletes)
	}
	if len(report.Deletes.Invalid) != 1 {
		t.Fatalf("delete invalid entries = %+v, want the torn tail reported", report.Deletes.Invalid)
	}
	if len(report.Deletes.Records) != 1 {
		t.Fatalf("delete records = %+v, want the record the delete did not commit", report.Deletes.Records)
	}
	record := report.Deletes.Records[0]
	if record.Path != "/baidu/x.bin" || record.Attempts != 1 || record.Error != "baidu api errno 12" || !record.UpdatedAt.Equal(updated) {
		t.Fatalf("delete record = %+v, want the failed /baidu/x.bin attempt", record)
	}
	// The upload journal's own counts must not absorb the delete records.
	if report.Entries != 0 || len(report.Pending) != 0 {
		t.Fatalf("upload journal counts changed: entries=%d pending=%+v", report.Entries, report.Pending)
	}

	var out strings.Builder
	PrintReport(&out, report)
	for _, want := range []string{"delete journal file=deletes.jsonl entries=1 failed=1 invalid=1", "/baidu/x.bin", "baidu api errno 12"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report output missing %q:\n%s", want, out.String())
		}
	}
}
