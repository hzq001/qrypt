package delete

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/yinzhenyu/qrypt/pkg/drive"
)

func journalEntryFor() drive.Entry {
	return drive.Entry{
		ID:    "id-1",
		Name:  "x.bin",
		Size:  12,
		Extra: drive.EntryExtraWrapper{RemoteName: "enc-x.bin"},
	}
}

// TestJournalRecordsAttemptsAndSurvivesReopen pins the durability contract: a
// queued delete, its spent attempts and its failure text outlive the process
// that recorded them, and a committed delete drops the record for good.
func TestJournalRecordsAttemptsAndSurvivesReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	entry := journalEntryFor()
	if err := journal.Put(RecordFor("/x.bin", entry)); err != nil {
		t.Fatalf("put intent: %v", err)
	}
	if records := journal.Records(); len(records) != 1 || records[0].Path != "/x.bin" {
		t.Fatalf("records after put = %+v, want the queued intent", records)
	}
	if record, ok := journal.Record("/x.bin"); !ok || record.Error != "" {
		t.Fatalf("a queued delete has no failure text yet: %+v (ok=%t)", record, ok)
	}
	attempts, err := journal.RecordAttempt("/x.bin", entry, errors.New("baidu api errno 12: file not exist"))
	if err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	reopened, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	defer reopened.Close()
	record, ok := reopened.Record("/x.bin")
	if !ok {
		t.Fatal("queued delete did not survive reopen")
	}
	if record.Attempts != 1 || record.ID != "id-1" || record.Name != "x.bin" || record.Size != 12 {
		t.Fatalf("reopened record = %+v, want the attempt and entry fields preserved", record)
	}
	if record.RemoteName != "enc-x.bin" {
		t.Fatalf("remote name = %q, want the encrypted name an encrypted mount addresses", record.RemoteName)
	}
	if !strings.Contains(record.Error, "errno 12") {
		t.Fatalf("error = %q, want the driver's failure text", record.Error)
	}
	rebuilt := record.Entry()
	if remoteName, ok := drive.EntryRemoteName(rebuilt); !ok || remoteName != "enc-x.bin" {
		t.Fatalf("rebuilt entry remote name = %q (ok=%t), want enc-x.bin", remoteName, ok)
	}
	if rebuilt.ID != "id-1" || rebuilt.Name != "x.bin" || rebuilt.IsDir || rebuilt.Size != 12 {
		t.Fatalf("rebuilt entry = %+v, want the recorded entry", rebuilt)
	}
	if record, ok := reopened.Record("/x.bin"); !ok || record.Error == "" {
		t.Fatalf("reopened record = %+v (ok=%t), want the recorded failure text", record, ok)
	}

	if err := reopened.Done("/x.bin"); err != nil {
		t.Fatalf("clear record: %v", err)
	}
	if _, ok := reopened.Record("/x.bin"); ok {
		t.Fatal("cleared record still present before reopen")
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened journal: %v", err)
	}
	again, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("reopen after clear: %v", err)
	}
	defer again.Close()
	if _, ok := again.Record("/x.bin"); ok {
		t.Fatal("cleared record came back after reopen")
	}
}

// TestJournalDoneUnderKeepsDirectoryIntent covers the directory paths: a
// directory delete or takeover voids its children's queued deletes while the
// directory's own record stays.
func TestJournalDoneUnderKeepsDirectoryIntent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer journal.Close()
	entry := journalEntryFor()
	for _, path := range []string{"/dir", "/dir/a.bin", "/dir/deep/b.bin", "/other.bin"} {
		if err := journal.Put(RecordFor(path, entry)); err != nil {
			t.Fatalf("put %s: %v", path, err)
		}
	}
	if err := journal.DoneUnder("/dir"); err != nil {
		t.Fatalf("done under: %v", err)
	}
	paths := make([]string, 0, 4)
	for _, record := range journal.Records() {
		paths = append(paths, record.Path)
	}
	want := []string{"/dir", "/other.bin"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("records after DoneUnder = %v, want %v", paths, want)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	reopened, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	defer reopened.Close()
	if len(reopened.Records()) != 2 {
		t.Fatalf("reopened records = %+v, want the two surviving records", reopened.Records())
	}
}

// TestJournalCompactsWithoutLosingRecords keeps the log bounded on a mount that
// deletes forever, without dropping the live intents.
func TestJournalCompactsWithoutLosingRecords(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	journal, err := openJournal(dir, 4)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer journal.Close()
	entry := journalEntryFor()
	for i := range 20 {
		if _, err := journal.RecordAttempt("/x.bin", entry, errors.New("network is unreachable")); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if err := journal.Put(RecordFor("/y.bin", entry)); err != nil {
		t.Fatalf("put second record: %v", err)
	}
	data, err := os.ReadFile(journal.Path())
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1
	if lines > 8 {
		t.Fatalf("journal kept %d lines for 2 records, want a compacted log", lines)
	}
	records := journal.Records()
	if len(records) != 2 {
		t.Fatalf("records after compaction = %+v, want both records", records)
	}
	if records[0].Attempts != 20 {
		t.Fatalf("attempts after compaction = %d, want 20", records[0].Attempts)
	}
}

// TestJournalSkipsCorruptTail is what a crash mid-append looks like: the
// records that did decode must still load.
func TestJournalSkipsCorruptTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	entry := journalEntryFor()
	if err := journal.Put(RecordFor("/x.bin", entry)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	f, err := os.OpenFile(journal.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(`{"op":"put","record":{"path":"/torn`); err != nil {
		t.Fatalf("write torn line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close appended file: %v", err)
	}

	reopened, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	defer reopened.Close()
	if _, ok := reopened.Record("/x.bin"); !ok {
		t.Fatal("a corrupt tail dropped the records before it")
	}
}

// TestOpenJournalRequiresDirectory keeps the misconfiguration loud instead of
// writing intents into the process working directory.
func TestOpenJournalRequiresDirectory(t *testing.T) {
	t.Parallel()
	if _, err := OpenJournal(""); err == nil {
		t.Fatal("OpenJournal(\"\") must fail")
	}
}
