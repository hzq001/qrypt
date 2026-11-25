package fs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yinzhenyu/qrypt/internal/cli/i18n"
	cliruntime "github.com/yinzhenyu/qrypt/internal/cli/runtime"
	"github.com/yinzhenyu/qrypt/pkg/vfs"
	"github.com/yinzhenyu/qrypt/pkg/vfs/vfstypes"
)

func NewStatCmd(rt cliruntime.Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "stat REMOTE",
		Short:             "Show path metadata",
		Long:              `Show the size, type, and modification time of REMOTE.`,
		Example:           `  qrypt fs stat /cloud/photos/cat.jpg`,
		Args:              cliruntime.ExactNamedArgs(rt, "REMOTE"),
		RunE:              runStat(rt),
		ValidArgsFunction: cliruntime.NoFileCompletions,
	}
	cmd.Flags().Bool("json", false, "write JSON output")
	return cmd
}

func runStat(rt cliruntime.Runtime) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, fs, cleanup, err := openFS(rt, cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		entry, err := fs.Stat(ctx, args[0])
		if err != nil {
			return err
		}
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			return cliruntime.WritePrettyJSON(cmd.OutOrStdout(), entry)
		}
		cliruntime.PrintEntryStat(cmd.OutOrStdout(), i18n.FromCommand(cmd), entry)
		return nil
	}
}

func NewMkdirCmd(rt cliruntime.Runtime) *cobra.Command {
	return &cobra.Command{
		Use:               "mkdir REMOTE",
		Short:             "Create a directory",
		Long:              `Create the directory REMOTE.`,
		Example:           `  qrypt fs mkdir /cloud/photos/2026`,
		Args:              cliruntime.ExactNamedArgs(rt, "REMOTE"),
		RunE:              runMkdir(rt),
		ValidArgsFunction: cliruntime.NoFileCompletions,
	}
}

func runMkdir(rt cliruntime.Runtime) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, fs, cleanup, err := openFS(rt, cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		_, err = fs.Mkdir(ctx, args[0])
		return err
	}
}

func NewRmCmd(rt cliruntime.Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "rm REMOTE",
		Short:             "Remove a file or empty directory",
		Long:              `Remove the file or empty directory REMOTE, waiting for the deletion to reach the backend.`,
		Example:           `  qrypt fs rm /cloud/docs/old-report.pdf`,
		Args:              cliruntime.ExactNamedArgs(rt, "REMOTE"),
		RunE:              runRm(rt),
		ValidArgsFunction: cliruntime.NoFileCompletions,
	}
	cmd.Flags().Duration("wait-timeout", 30*time.Second, "maximum time to wait for deletion to finish")
	cmd.Flags().Bool("json", false, "write JSON output")
	return cmd
}

func runRm(rt cliruntime.Runtime) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		waitTimeout := cliruntime.CommandWaitTimeout(cmd)
		if waitTimeout <= 0 {
			return fmt.Errorf("--wait-timeout must be greater than 0")
		}
		requested := vfstypes.CleanVirtualPath(args[0])
		ctx, fs, cleanup, err := openFS(rt, cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		entry, err := fs.Stat(ctx, args[0])
		if err != nil {
			return err
		}
		if entry.IsDir {
			err = fs.RemoveDir(ctx, args[0])
		} else {
			err = fs.Remove(ctx, args[0])
		}
		if err != nil {
			return err
		}
		// The remote delete runs in the background: the wait only says the
		// filesystem stopped working, not that the backend accepted the
		// delete. Ask the filesystem what it recorded, so a delete the driver
		// refused is reported instead of answered with a success exit code.
		waitErr := waitPathSettled(ctx, fs, args[0], waitTimeout)
		failures := remoteDeleteFailures(fs, requested, entry.IsDir)
		if len(failures) == 0 {
			if waitErr != nil {
				return waitErr
			}
			asJSON, _ := cmd.Flags().GetBool("json")
			if asJSON {
				return cliruntime.WritePrettyJSON(cmd.OutOrStdout(), rmResult{Path: args[0], Removed: true})
			}
			fmt.Fprintln(cmd.OutOrStdout(), i18n.FromCommand(cmd).T("out.fs.rm.removed", args[0]))
			return nil
		}
		rmErr := rmFailureError(failures, waitErr)
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			// The structured result describes the same outcome as the error
			// returned below: --json changes the shape of the output, not the
			// exit status (fs copy reports partial failures the same way).
			result := rmResult{Path: args[0], Removed: false, Failures: rmFailures(failures)}
			if err := cliruntime.WritePrettyJSON(cmd.OutOrStdout(), result); err != nil {
				return err
			}
		}
		return rmFailureResult(rt, requested, failures, rmErr)
	}
}

// rmResult is the --json shape of `fs rm`. Failures holds one entry per path
// whose remote delete did not commit; there can be several below a removed
// directory. removed stays true only when nothing failed: claiming success
// for a delete the backend refused is the bug this shape exists to prevent.
type rmResult struct {
	Path     string      `json:"path"`
	Removed  bool        `json:"removed"`
	Failures []rmFailure `json:"failures,omitempty"`
}

// rmFailure projects vfs.FailedRemoteDelete for the rm --json result. pending
// means another attempt is still armed and will retry in the background.
type rmFailure struct {
	Path     string `json:"path"`
	Error    string `json:"error"`
	Attempts int    `json:"attempts"`
	Pending  bool   `json:"pending"`
}

func rmFailures(failures []vfs.FailedRemoteDelete) []rmFailure {
	out := make([]rmFailure, 0, len(failures))
	for _, failure := range failures {
		out = append(out, rmFailure{
			Path:     failure.Path,
			Error:    failure.Error,
			Attempts: failure.Attempts,
			Pending:  failure.Pending,
		})
	}
	return out
}

// remoteDeleteFailures returns the recorded delete failures that cover the
// path the user removed: the path itself, plus - for a directory - everything
// strictly below it, because a recursive remove that leaves a child in the
// backend has to be reported even though the child is a different path.
// Reported paths live in the same namespace as the command's arguments (both
// carry the /MOUNT/ prefix), so they compare directly once cleaned. Sorted by
// path so the message is stable across runs.
func remoteDeleteFailures(fs vfs.FileSystem, requested string, isDir bool) []vfs.FailedRemoteDelete {
	inspector, ok := fs.(vfs.DeleteInspector)
	if !ok {
		return nil
	}
	var matches []vfs.FailedRemoteDelete
	for _, failure := range inspector.FailedRemoteDeletes() {
		if vfstypes.CleanVirtualPath(failure.Path) == requested ||
			(isDir && vfstypes.IsPathUnder(failure.Path, requested)) {
			matches = append(matches, failure)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Path < matches[j].Path })
	return matches
}

// rmFailureError renders the recorded failures as one error. The driver's
// text is the only trace of why the backend refused, so it is reproduced
// verbatim; will retry=false means nothing else will be tried (the attempt
// budget is spent, or the failure is deterministic, like Baidu's errno 12).
func rmFailureError(failures []vfs.FailedRemoteDelete, waitErr error) error {
	if len(failures) == 1 {
		failure := failures[0]
		return withWaitError(fmt.Errorf("remote delete failed for %s: %s (attempts=%d, will retry=%v)",
			failure.Path, deleteFailureText(failure), failure.Attempts, failure.Pending), waitErr)
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "remote delete failed for %d path(s):", len(failures))
	for _, failure := range failures {
		fmt.Fprintf(&msg, "\n  %s: %s (attempts=%d, will retry=%v)",
			failure.Path, deleteFailureText(failure), failure.Attempts, failure.Pending)
	}
	return withWaitError(errors.New(msg.String()), waitErr)
}

// deleteFailureText renders what the backend said about one uncommitted delete.
// A record without error text was never attempted - it was still queued when
// the wait ran out - and saying so beats an empty gap after the colon.
func deleteFailureText(failure vfs.FailedRemoteDelete) string {
	if failure.Error != "" {
		return failure.Error
	}
	return "not attempted yet"
}

// withWaitError keeps the idle-wait failure next to the driver's error when
// both happened: the wait says why the CLI stopped waiting (it may well have
// timed out on a retry the failure text says is still armed), the recorded
// failure says what the backend actually did.
func withWaitError(err, waitErr error) error {
	if waitErr == nil {
		return err
	}
	return fmt.Errorf("%s (wait: %w)", err, waitErr)
}

// rmFailureResult shapes the exit status of a delete that did not commit. A
// failure on the requested path itself means nothing was removed remotely:
// that is the plain error (exit 1) `fs copy` returns for a single file that
// did not copy. Failures strictly below a recursive remove leave the local
// view dropped while some remote entries stayed behind: that is the partial
// case `fs copy -r` and `fs sync` report as exit 3 so a script can rerun only
// the failed subset.
func rmFailureResult(rt cliruntime.Runtime, requested string, failures []vfs.FailedRemoteDelete, err error) error {
	// An interrupted wait (Ctrl-C) keeps its own exit status, so it is
	// returned untagged; the recorded failure text still rides along.
	if errors.Is(err, context.Canceled) {
		return err
	}
	for _, failure := range failures {
		if vfstypes.CleanVirtualPath(failure.Path) == requested {
			return err
		}
	}
	return rt.ExitError(cliruntime.ExitPartial, err)
}

func NewMvCmd(rt cliruntime.Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "mv SOURCE DESTINATION",
		Short:             "Rename or move a path",
		Long:              `Rename or move SOURCE to DESTINATION.`,
		Example:           `  qrypt fs mv /cloud/docs/draft.pdf /cloud/docs/final.pdf`,
		Args:              cliruntime.ExactNamedArgs(rt, "SOURCE", "DESTINATION"),
		RunE:              runMv(rt),
		ValidArgsFunction: cliruntime.NoFileCompletions,
	}
	cmd.Flags().Bool("json", false, "write JSON output")
	return cmd
}

func runMv(rt cliruntime.Runtime) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, fs, cleanup, err := openFS(rt, cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		if err := fs.Rename(ctx, args[0], args[1]); err != nil {
			return err
		}
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			return cliruntime.WritePrettyJSON(cmd.OutOrStdout(), struct {
				Source      string `json:"source"`
				Destination string `json:"destination"`
				Renamed     bool   `json:"renamed"`
			}{Source: args[0], Destination: args[1], Renamed: true})
		}
		fmt.Fprintln(cmd.OutOrStdout(), i18n.FromCommand(cmd).T("out.fs.mv.renamed", args[0], args[1]))
		return nil
	}
}
