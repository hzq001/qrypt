package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	cliruntime "github.com/yinzhenyu/qrypt/internal/cli/runtime"
	"github.com/yinzhenyu/qrypt/pkg/config"
	"github.com/yinzhenyu/qrypt/pkg/core"
	"github.com/yinzhenyu/qrypt/pkg/util"
	"github.com/yinzhenyu/qrypt/pkg/vfs"
	idelete "github.com/yinzhenyu/qrypt/pkg/vfs/delete"
)

// Runtime is the slice of Runtime this package needs: config
// resolution, cached-journal paths, usage errors, and the debug schema.
// journal commands never build or open a filesystem, so they depend on this
// narrow surface instead of the full Runtime.
type Runtime interface {
	cliruntime.FileSystemBuilder
	cliruntime.ErrorFactory
	cliruntime.DebugReporter
}

type DebugCacheTarget struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

type DebugReport struct {
	Target         DebugCacheTarget `json:"target"`
	Entries        int              `json:"entries"`
	DirtyEntries   int              `json:"dirty_entries"`
	CleanEntries   int              `json:"clean_entries"`
	InvalidEntries []InvalidEntry   `json:"invalid_entries"`
	Pending        []PendingDebug   `json:"pending"`
	PermanentFails int              `json:"permanent_fails"`
	OrphanStaging  []string         `json:"orphan_staging"`
	Deletes        DeleteReport     `json:"deletes"`
}

// DeleteReport is the read-only view of a mount's delete journal
// (<mount>/deletes.jsonl). It is reported next to the upload journal rather
// than inside it because the two files record different intents: pending.jsonl
// holds uploads and deletes.jsonl holds remote deletes that need a live driver.
type DeleteReport struct {
	// File names the journal this report came from, so a reader can tell
	// deletes.jsonl from pending.jsonl without knowing the layout.
	File    string         `json:"file"`
	Entries int            `json:"entries"`
	Failed  int            `json:"failed"`
	Invalid []InvalidEntry `json:"invalid_entries"`
	Records []DeleteDebug  `json:"records"`
}

// DeleteDebug is one live delete-journal record: a path whose remote delete is
// queued, being attempted, or failed. The journal keeps no staging data - the
// record is the whole intent - so there is nothing else to check.
type DeleteDebug struct {
	Path      string    `json:"path"`
	Attempts  int       `json:"attempts"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type InvalidEntry struct {
	Line int    `json:"line"`
	Err  string `json:"err"`
}

type PendingDebug struct {
	vfs.PendingUpload
	StagingExists bool   `json:"staging_exists"`
	StagingSize   int64  `json:"staging_size"`
	StagingError  string `json:"staging_error,omitempty"`
}

type debugJournalEntry struct {
	Op string `json:"op"`
	vfs.PendingUpload
}

func NewCommand(rt Runtime, use string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: "Inspect the offline upload journal",
		Long: `Inspect the upload journal owned by the resident Core. The journal is
read-only from the CLI; maintenance moves to the Core's operation workflow.`,
		Example: `  qrypt fs journal
  qrypt fs journal --mount cloud
  qrypt fs journal --json`,
		Args: cliruntime.NoArgs(rt),
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := TargetsFromCmd(rt, cmd)
			if err != nil {
				return err
			}
			asJSON, _ := cmd.Flags().GetBool("json")
			var reports []DebugReport
			for i, target := range targets {
				if !asJSON && i > 0 {
					fmt.Fprintln(cmd.OutOrStdout())
				}
				report := InspectCache(target)
				if asJSON {
					reports = append(reports, report)
				} else {
					PrintReport(cmd.OutOrStdout(), report)
				}
			}
			if asJSON {
				return WriteReportsJSON(rt, cmd.OutOrStdout(), reports)
			}
			return nil
		},
	}
	addJournalTargetFlags(cmd)
	cmd.AddCommand(newRemovedMaintenanceCmd("replay"))
	cmd.AddCommand(newRemovedMaintenanceCmd("prune"))
	return cmd
}

// journalTargetsFromCmd resolves the journal directories to inspect from the
// config (or explicit --cache-dir/--mount flags).
func TargetsFromCmd(rt Runtime, cmd *cobra.Command) ([]DebugCacheTarget, error) {
	cacheDir := cliruntime.FlagStringValue(cmd, "cache-dir")
	mountName := cliruntime.FlagStringValue(cmd, "mount")
	state, err := rt.CommandConfig(cmd)
	if err != nil {
		return nil, err
	}
	if state.Cfg == nil && cacheDir == "" {
		return nil, fmt.Errorf("%w; alternatively use --cache-dir", rt.ConfigNotFoundError())
	}
	if state.Cfg == nil && mountName != "" {
		return nil, fmt.Errorf("--mount requires a config file")
	}
	return DebugCacheTargets(rt, cacheDir, state.Cfg, mountName)
}

func addJournalTargetFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().String("mount", "", "mount name")
	cmd.PersistentFlags().String("cache-dir", "", "cache directory")
	cmd.Flags().Bool("json", false, "write JSON output")
}

// newRemovedMaintenanceCmd keeps the old entry points callable long enough to
// explain where journal maintenance moved, without advertising them in help.
func newRemovedMaintenanceCmd(use string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("journal maintenance moved to the resident Core; `qrypt core ops reconcile` will replace `qrypt fs journal %s`", use)
		},
	}
}

func WriteReportsJSON(rt Runtime, w io.Writer, reports []DebugReport) error {
	return cliruntime.WritePrettyJSON(w, struct {
		SchemaVersion int           `json:"schema_version"`
		GeneratedAt   time.Time     `json:"generated_at"`
		Reports       []DebugReport `json:"reports"`
	}{
		SchemaVersion: rt.DebugReportSchemaVersion(),
		GeneratedAt:   time.Now(),
		Reports:       reports,
	})
}

func DebugCacheTargets(rt Runtime, cacheDir string, cfg *config.Config, mountName string) ([]DebugCacheTarget, error) {
	if cfg == nil {
		if cacheDir == "" {
			cacheDir = rt.DefaultCacheDir()
		}
		return []DebugCacheTarget{{Name: "default", Dir: util.ExpandHome(cacheDir)}}, nil
	}
	baseStorageDir := core.NewStorageLayout(cfg, core.RuntimeLayout{}).UploadDir
	if cacheDir != "" {
		baseStorageDir = cacheDir
	}
	if baseStorageDir != "" {
		baseStorageDir = util.ExpandHome(baseStorageDir)
	}
	if len(cfg.Mounts) == 0 {
		return []DebugCacheTarget{{Name: "default", Dir: baseStorageDir}}, nil
	}
	var targets []DebugCacheTarget
	for _, mount := range cfg.Mounts {
		if mountName != "" && mount.Name != mountName {
			continue
		}
		dir := filepath.Join(baseStorageDir, mount.Name)
		targets = append(targets, DebugCacheTarget{Name: mount.Name, Dir: dir})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("config: no mounts selected")
	}
	return targets, nil
}

func InspectCache(target DebugCacheTarget) DebugReport {
	report := DebugReport{
		Target:         target,
		InvalidEntries: []InvalidEntry{},
		Pending:        []PendingDebug{},
		OrphanStaging:  []string{},
	}
	journalPath := filepath.Join(target.Dir, "pending.jsonl")
	file, err := os.Open(journalPath)
	if err == nil {
		defer file.Close()
		pending := map[string]vfs.PendingUpload{}
		scanner := bufio.NewScanner(file)
		line := 0
		for scanner.Scan() {
			line++
			var entry debugJournalEntry
			if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
				report.InvalidEntries = append(report.InvalidEntries, InvalidEntry{Line: line, Err: err.Error()})
				continue
			}
			report.Entries++
			switch entry.Op {
			case "dirty":
				report.DirtyEntries++
				if entry.LocalPath != "" {
					pending[entry.Path] = entry.PendingUpload
				}
			case "clean":
				report.CleanEntries++
				delete(pending, entry.Path)
			default:
				report.InvalidEntries = append(report.InvalidEntries, InvalidEntry{Line: line, Err: "unknown op " + entry.Op})
			}
		}
		if err := scanner.Err(); err != nil {
			report.InvalidEntries = append(report.InvalidEntries, InvalidEntry{Line: line + 1, Err: err.Error()})
		}
		for _, item := range pending {
			debug := PendingDebug{PendingUpload: item}
			if item.PermanentFail {
				report.PermanentFails++
			}
			status, size := cliruntime.StagingStatus(item)
			debug.StagingExists = status == "ok" || status == "size-mismatch"
			debug.StagingSize = size
			if status == "missing" {
				debug.StagingError = "missing"
			}
			report.Pending = append(report.Pending, debug)
		}
	}
	sort.Slice(report.Pending, func(i, j int) bool {
		return report.Pending[i].Path < report.Pending[j].Path
	})
	report.OrphanStaging = OrphanStagingFiles(target.Dir, report.Pending)
	if report.OrphanStaging == nil {
		report.OrphanStaging = []string{}
	}
	report.Deletes = InspectDeleteJournal(filepath.Join(target.Dir, idelete.JournalName))
	return report
}

// deleteJournalEntry is one line of deletes.jsonl: the put/done envelope
// pkg/vfs/delete writes plus the record it carries. The envelope is re-read
// here instead of opening the journal through pkg/vfs/delete, because opening
// it creates the directory and fsyncs - which an inspect command must not do
// to a mount that has never deleted anything.
type deleteJournalEntry struct {
	Op     string                 `json:"op"`
	Path   string                 `json:"path,omitempty"`
	Record *idelete.JournalRecord `json:"record,omitempty"`
}

// InspectDeleteJournal replays a delete journal into its live records. It
// understands the same ops as the journal's own loader, so a report computed
// while the mount is running matches what a mount start would replay.
func InspectDeleteJournal(path string) DeleteReport {
	report := DeleteReport{File: idelete.JournalName, Invalid: []InvalidEntry{}, Records: []DeleteDebug{}}
	file, err := os.Open(path)
	if err != nil {
		return report
	}
	defer file.Close()

	records := map[string]idelete.JournalRecord{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var entry deleteJournalEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			report.Invalid = append(report.Invalid, InvalidEntry{Line: line, Err: err.Error()})
			continue
		}
		switch entry.Op {
		case "put":
			if entry.Record == nil || entry.Record.Path == "" {
				report.Invalid = append(report.Invalid, InvalidEntry{Line: line, Err: "put without record"})
				continue
			}
			records[entry.Record.Path] = *entry.Record
		case "done":
			delete(records, entry.Path)
		case "done_under":
			prefix := strings.TrimSuffix(entry.Path, "/") + "/"
			for recordPath := range records {
				if strings.HasPrefix(recordPath, prefix) {
					delete(records, recordPath)
				}
			}
		default:
			report.Invalid = append(report.Invalid, InvalidEntry{Line: line, Err: "unknown op " + entry.Op})
		}
	}
	if err := scanner.Err(); err != nil {
		report.Invalid = append(report.Invalid, InvalidEntry{Line: line + 1, Err: err.Error()})
	}
	for _, record := range records {
		if record.Error != "" {
			report.Failed++
		}
		report.Records = append(report.Records, DeleteDebug{
			Path:      record.Path,
			Attempts:  record.Attempts,
			Error:     record.Error,
			UpdatedAt: record.UpdatedAt,
		})
	}
	report.Entries = len(report.Records)
	sort.Slice(report.Records, func(i, j int) bool { return report.Records[i].Path < report.Records[j].Path })
	return report
}

func OrphanStagingFiles(cacheDir string, pending []PendingDebug) []string {
	known := map[string]bool{}
	for _, item := range pending {
		if item.LocalPath != "" {
			known[item.LocalPath] = true
		}
	}
	files, err := filepath.Glob(filepath.Join(cacheDir, "staging", "*.staging"))
	if err != nil {
		return nil
	}
	var orphans []string
	for _, file := range files {
		if !known[file] {
			orphans = append(orphans, file)
		}
	}
	sort.Strings(orphans)
	return orphans
}

func PrintReport(w io.Writer, report DebugReport) {
	fmt.Fprintf(w, "cache %s %s\n", report.Target.Name, report.Target.Dir)
	fmt.Fprintf(w, "journal entries=%d dirty=%d clean=%d invalid=%d pending=%d permanent_fail=%d orphan_staging=%d\n",
		report.Entries, report.DirtyEntries, report.CleanEntries,
		len(report.InvalidEntries), len(report.Pending), report.PermanentFails, len(report.OrphanStaging))
	for _, inv := range report.InvalidEntries {
		fmt.Fprintf(w, "  invalid line %d: %s\n", inv.Line, inv.Err)
	}
	if len(report.Pending) > 0 {
		fmt.Fprintln(w, "pending:")
		for _, item := range report.Pending {
			fmt.Fprintf(w, "  %s size=%d staging_exists=%v staging_size=%d staging_error=%q permanent_fail=%v last_error=%q\n",
				item.Path, item.Size, item.StagingExists, item.StagingSize, item.StagingError, item.PermanentFail, item.LastError)
		}
	}
	if len(report.OrphanStaging) > 0 {
		fmt.Fprintln(w, "orphan staging files:")
		for _, name := range report.OrphanStaging {
			fmt.Fprintf(w, "  %s\n", name)
		}
	}
	fmt.Fprintf(w, "delete journal file=%s entries=%d failed=%d invalid=%d\n",
		report.Deletes.File, report.Deletes.Entries, report.Deletes.Failed, len(report.Deletes.Invalid))
	for _, inv := range report.Deletes.Invalid {
		fmt.Fprintf(w, "  delete invalid line %d: %s\n", inv.Line, inv.Err)
	}
	if len(report.Deletes.Records) > 0 {
		fmt.Fprintln(w, "delete records:")
		for _, record := range report.Deletes.Records {
			fmt.Fprintf(w, "  %s attempts=%d error=%q updated=%s\n",
				record.Path, record.Attempts, record.Error, record.UpdatedAt.UTC().Format(time.RFC3339))
		}
	}
}
