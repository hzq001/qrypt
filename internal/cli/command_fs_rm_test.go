package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinzhenyu/qrypt/pkg/config"
	"github.com/yinzhenyu/qrypt/pkg/control"
	corepkg "github.com/yinzhenyu/qrypt/pkg/core"
	"github.com/yinzhenyu/qrypt/pkg/core/ownerlock"
	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/drivers/localfs"
	"github.com/yinzhenyu/qrypt/pkg/fsclient"
	"github.com/yinzhenyu/qrypt/pkg/task"
	"github.com/yinzhenyu/qrypt/pkg/util"
	"github.com/yinzhenyu/qrypt/pkg/vfs"
	idelete "github.com/yinzhenyu/qrypt/pkg/vfs/delete"
)

// driverDeleteError is the text the motivating field failure produced: Baidu
// answers errno 12, a deterministic failure, so no retry is scheduled for it.
const driverDeleteError = "baidu api errno 12: unknown"

type rmTaskControllerStub struct{}

func (rmTaskControllerStub) CancelTask(context.Context, string) error { return nil }
func (rmTaskControllerStub) RetryTask(context.Context, string) error  { return nil }

type rmRuntimeService struct {
	configPath  string
	fingerprint string
	workDir     string
	startedAt   time.Time
}

func (s rmRuntimeService) Status(context.Context) (control.RuntimeStatus, error) {
	return control.RuntimeStatus{
		PID:             os.Getpid(),
		ProtocolVersion: control.ProtocolVersion,
		ConfigPath:      s.configPath,
		Fingerprint:     s.fingerprint,
		WorkDir:         s.workDir,
		StartedAt:       s.startedAt,
	}, nil
}

func (s rmRuntimeService) Shutdown(context.Context) error { return nil }

var rmActiveTaskStates = []task.State{
	task.StateQueued,
	task.StateScheduled,
	task.StateRunning,
	task.StateRetryWait,
	task.StateCanceling,
}

func rmMatchesPathOrDescendant(candidate, target string) bool {
	clean := vfs.CleanVirtualPath(candidate)
	if clean == target {
		return true
	}
	if target == "/" {
		return strings.HasPrefix(clean, "/")
	}
	return strings.HasPrefix(clean, target+"/")
}

// rmNamespaceFS adapts a real *vfs.Namespace to control.FSService so CLI `fs rm`
// commands in tests exercise the real control HTTP server, fsclient, and VFS
// pipeline while allowing individual cases to inject wait/failure overrides.
type rmNamespaceFS struct {
	ns       *vfs.Namespace
	waitHook func(ctx context.Context, path string, timeout time.Duration) error
	failures []vfs.FailedRemoteDelete
	override bool
}

func (s *rmNamespaceFS) Stat(ctx context.Context, path string) (control.FSEntry, error) {
	entry, err := s.ns.Stat(ctx, path)
	if err != nil {
		return control.FSEntry{}, err
	}
	return control.FSEntry{
		ID:       entry.ID,
		ParentID: entry.ParentID,
		Name:     entry.Name,
		Path:     path,
		IsDir:    entry.IsDir,
		Size:     entry.Size,
		ModTime:  entry.ModTime,
	}, nil
}

func (s *rmNamespaceFS) List(ctx context.Context, path string) ([]control.FSEntry, error) {
	entries, err := s.ns.List(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]control.FSEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, control.FSEntry{
			ID:       entry.ID,
			ParentID: entry.ParentID,
			Name:     entry.Name,
			IsDir:    entry.IsDir,
			Size:     entry.Size,
			ModTime:  entry.ModTime,
		})
	}
	return out, nil
}

func (s *rmNamespaceFS) ReadStream(context.Context, string, int64, int64, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (s *rmNamespaceFS) Create(ctx context.Context, path string) error {
	return s.ns.Create(ctx, path)
}

func (s *rmNamespaceFS) WriteAt(ctx context.Context, path string, offset int64, data []byte) (int, error) {
	return s.ns.WriteAt(ctx, path, data, offset)
}

func (s *rmNamespaceFS) Flush(ctx context.Context, path string) error {
	return s.ns.Flush(ctx, path)
}

func (s *rmNamespaceFS) Mkdir(ctx context.Context, path string) (control.FSEntry, error) {
	entry, err := s.ns.Mkdir(ctx, path)
	if err != nil {
		return control.FSEntry{}, err
	}
	return control.FSEntry{ID: entry.ID, Name: entry.Name, Path: path, IsDir: entry.IsDir}, nil
}

func (s *rmNamespaceFS) SetModTime(context.Context, string, time.Time) error { return nil }

func (s *rmNamespaceFS) Remove(ctx context.Context, path string, dir bool) error {
	if dir {
		return s.ns.RemoveDir(ctx, path)
	}
	return s.ns.Remove(ctx, path)
}

func (s *rmNamespaceFS) Rename(ctx context.Context, source, destination string) error {
	return s.ns.Rename(ctx, source, destination)
}

func (s *rmNamespaceFS) Truncate(ctx context.Context, path string, size int64) error {
	return s.ns.Truncate(ctx, path, size)
}

func (s *rmNamespaceFS) WaitPath(ctx context.Context, path string, timeout time.Duration) error {
	if s.waitHook != nil {
		return s.waitHook(ctx, path, timeout)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cleanPath := vfs.CleanVirtualPath(path)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	src := s.ns.TaskSource()
	for {
		active := 0
		if src != nil {
			tasks, err := src.ListTasks(ctx, task.Filter{States: rmActiveTaskStates})
			if err != nil {
				return err
			}
			for _, item := range tasks {
				if rmMatchesPathOrDescendant(item.Path, cleanPath) {
					active++
				}
			}
		}
		if active == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("core: timed out waiting for %s to settle (tasks=%d)", path, active)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *rmNamespaceFS) WaitIdle(context.Context, time.Duration) error { return nil }
func (s *rmNamespaceFS) InvalidateList(context.Context, string) error  { return nil }
func (s *rmNamespaceFS) Space(context.Context) (control.FSSpace, error) {
	return control.FSSpace{}, nil
}
func (s *rmNamespaceFS) MountSpaces(context.Context) ([]control.FSMountSpace, error) {
	return nil, nil
}
func (s *rmNamespaceFS) PendingUploads(ctx context.Context) ([]vfs.PendingUpload, error) {
	return s.ns.PendingUploads(), nil
}
func (s *rmNamespaceFS) Copy(context.Context, control.FSCopyRequest) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (s *rmNamespaceFS) Resolve(ctx context.Context, path string) (control.FSResolveResult, error) {
	return control.FSResolveResult{Path: path}, nil
}

type rmInspectableFS struct {
	*rmNamespaceFS
}

func (s *rmInspectableFS) FailedDeletes(context.Context) ([]vfs.FailedRemoteDelete, error) {
	if s.override {
		return s.failures, nil
	}
	return s.ns.FailedRemoteDeletes(), nil
}

type rmSpyDriver struct {
	drive.Driver
	mu       sync.Mutex
	calls    int
	activeCh chan struct{}
	removeFn func(ctx context.Context, entry drive.Entry) error
}

func (d *rmSpyDriver) Remove(ctx context.Context, entry drive.Entry) error {
	d.mu.Lock()
	d.calls++
	ch := d.activeCh
	d.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	if d.removeFn != nil {
		return d.removeFn(ctx, entry)
	}
	return d.Driver.Remove(ctx, entry)
}

func writeRmTestConfig(t *testing.T, base, remote, mountName string) (configPath string, cfg *config.Config) {
	t.Helper()
	workDir := filepath.Join(base, "work")
	configPath = filepath.Join(base, "qrypt.toml")
	content := `
mount_point = ` + util.TOMLPath(filepath.Join(base, "mnt")) + `
[storage]
work_dir = ` + util.TOMLPath(workDir) + `

[upload]
upload_delay = "10ms"
delete_delay = "10ms"

[[mounts]]
name = ` + fmt.Sprintf("%q", mountName) + `
type = "localfs"
[mounts.params]
root_path = ` + util.TOMLPath(remote) + `
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return configPath, loaded
}

func canonicalTestConfigPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

func startRmNamespace(t *testing.T, drv drive.Driver, storageDir string, deleteDelay time.Duration) (context.Context, *vfs.Namespace) {
	t.Helper()
	mountFS, err := vfs.New(drv, vfs.Options{
		StorageDir:  storageDir,
		UploadDelay: time.Hour,
		DeleteDelay: deleteDelay,
	})
	if err != nil {
		t.Fatal(err)
	}
	ns, err := vfs.NewNamespace([]vfs.Mount{{Name: "loc", FS: mountFS}})
	if err != nil {
		_ = mountFS.Close(context.Background())
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ns.Start(ctx)
	t.Cleanup(func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = ns.Close(closeCtx)
	})
	return ctx, ns
}

func startRmControlServer(t *testing.T, configPath string, cfg *config.Config, fsSvc control.FSService) string {
	t.Helper()
	runDir := corepkg.RuntimeRunDir(configPath, cfg)
	endpoint := corepkg.ControlSocketPath(cfg, corepkg.RuntimeLayout{RunDir: runDir})
	canonPath := canonicalTestConfigPath(t, configPath)
	fp, err := ownerlock.FingerprintFile(canonPath)
	if err != nil {
		t.Fatal(err)
	}
	server, err := control.NewCommandServer(endpoint, rmTaskControllerStub{})
	if err != nil {
		t.Fatal(err)
	}
	server.SetRuntimeService(rmRuntimeService{
		configPath:  canonPath,
		fingerprint: fp,
		workDir:     cfg.Storage.WorkDir,
		startedAt:   time.Now(),
	})
	server.SetFSService(fsSvc)
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = server.Close(closeCtx)
	})
	return endpoint
}

func seedRmTree(t *testing.T, remote string) {
	t.Helper()
	for _, dir := range []string{filepath.Join(remote, "dir"), filepath.Join(remote, "dirx")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(remote, "a.bin"):         "aaa",
		filepath.Join(remote, "dir", "b.bin"):  "bbb",
		filepath.Join(remote, "dirx", "a.bin"): "xxx",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func rmTestCommand(t *testing.T, failures []vfs.FailedRemoteDelete, waitErr error) (*cobra.Command, *bytes.Buffer, string) {
	t.Helper()
	base := shortTempDir(t)
	remote := filepath.Join(base, "remote")
	seedRmTestFiles := func() { seedRmTree(t, remote) }
	seedRmTestFiles()
	configPath, cfg := writeRmTestConfig(t, base, remote, "loc")
	ctx, ns := startRmNamespace(t, localfs.New(remote), filepath.Join(base, "vfs-storage"), time.Millisecond)

	// Queue child deletions before RemoveDir when testing child failures under /loc/dir
	// so WaitPath/takeover behavior matches production recursive rm fixtures (ff43d31).
	for _, f := range failures {
		if f.Path != "/loc/dir" && strings.HasPrefix(f.Path, "/loc/dir/") {
			_ = ns.Remove(ctx, f.Path)
		}
	}

	svc := &rmInspectableFS{
		rmNamespaceFS: &rmNamespaceFS{
			ns: ns,
			waitHook: func(context.Context, string, time.Duration) error {
				return waitErr
			},
			failures: failures,
			override: true,
		},
	}
	startRmControlServer(t, configPath, cfg, svc)

	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	return root, &out, configPath
}

func failedDelete(path string, attempts int, pending bool) vfs.FailedRemoteDelete {
	return vfs.FailedRemoteDelete{
		Path:      path,
		Error:     driverDeleteError,
		Attempts:  attempts,
		Pending:   pending,
		UpdatedAt: time.Now(),
	}
}

func TestFsRmThroughCoreNormalAndDirectory(t *testing.T) {
	t.Parallel()
	base := shortTempDir(t)
	configPath, _ := writeCoreServeTestConfig(t, base)
	remote := filepath.Join(base, "remote")
	if err := os.MkdirAll(filepath.Join(remote, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range map[string]string{
		"a.bin":     "aaa",
		"b.bin":     "bbb",
		"dir/c.bin": "ccc",
	} {
		if err := os.WriteFile(filepath.Join(remote, rel), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	serveRoot := NewRootCommand()
	var serveOut bytes.Buffer
	serveRoot.SetOut(&serveOut)
	serveRoot.SetErr(&serveOut)
	serveRoot.SetArgs([]string{"core", "serve", "--foreground", "--keep", "--config", configPath})
	done := make(chan error, 1)
	go func() { done <- serveRoot.Execute() }()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_, _ = executeRoot(t, "core", "stop", "--force", "--config", configPath)
		<-done
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := executeRoot(t, "core", "status", "--json", "--config", configPath)
		if err == nil && strings.Contains(out, `"state": "running"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("core did not become ready: err=%v out=%s serve=%s", err, out, serveOut.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	out, err := executeRoot(t, "fs", "--config", configPath, "rm", "/local/a.bin")
	if err != nil {
		t.Fatalf("fs rm via core: %v\n%s", err, out)
	}
	if out != "removed /local/a.bin\n" {
		t.Fatalf("stdout = %q, want removed line", out)
	}
	if _, err := os.Stat(filepath.Join(remote, "a.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote a.bin still present: %v", err)
	}

	out, err = executeRoot(t, "fs", "--config", configPath, "rm", "--json", "/local/b.bin")
	if err != nil {
		t.Fatalf("fs rm --json via core: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"removed": true`) || strings.Contains(out, `"failures"`) {
		t.Fatalf("rm --json output = %s", out)
	}

	if _, err := executeRoot(t, "fs", "--config", configPath, "rm", "/local/dir/c.bin"); err != nil {
		t.Fatalf("fs rm dir child via core: %v", err)
	}
	if _, err := executeRoot(t, "fs", "--config", configPath, "rm", "/local/dir"); err != nil {
		t.Fatalf("fs rm dir via core: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remote, "dir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote dir still present: %v", err)
	}
}

func TestFsRmFailsWhenRemoteDeleteWasRecorded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		failures []vfs.FailedRemoteDelete
		waitErr  error
		cancel   bool
		want     []string
		absent   []string
		wantCode int
	}{
		{
			name:     "file delete refused",
			args:     []string{"/loc/a.bin"},
			failures: []vfs.FailedRemoteDelete{failedDelete("/loc/a.bin", 1, false)},
			want: []string{
				"remote delete failed for /loc/a.bin: " + driverDeleteError + " (attempts=1, will retry=false)",
			},
			wantCode: ExitFailure,
		},
		{
			name:     "retry still armed",
			args:     []string{"/loc/a.bin"},
			failures: []vfs.FailedRemoteDelete{failedDelete("/loc/a.bin", 2, true)},
			want: []string{
				"remote delete failed for /loc/a.bin: " + driverDeleteError + " (attempts=2, will retry=true)",
			},
			wantCode: ExitFailure,
		},
		{
			name: "queued delete never attempted",
			args: []string{"/loc/a.bin"},
			failures: []vfs.FailedRemoteDelete{{
				Path:      "/loc/a.bin",
				Attempts:  0,
				Pending:   true,
				UpdatedAt: time.Now(),
			}},
			want: []string{
				"remote delete failed for /loc/a.bin: not attempted yet (attempts=0, will retry=true)",
			},
			wantCode: ExitFailure,
		},
		{
			name:     "trailing slash names the same file",
			args:     []string{"/loc/a.bin/"},
			failures: []vfs.FailedRemoteDelete{failedDelete("/loc/a.bin", 1, false)},
			want: []string{
				"remote delete failed for /loc/a.bin: " + driverDeleteError + " (attempts=1, will retry=false)",
			},
			wantCode: ExitFailure,
		},
		{
			name: "directory remove reports the children left behind",
			args: []string{"/loc/dir"},
			failures: []vfs.FailedRemoteDelete{
				failedDelete("/loc/dirx/a.bin", 1, false),
				failedDelete("/loc/dir/b.bin", 3, false),
			},
			want: []string{
				"remote delete failed for /loc/dir/b.bin: " + driverDeleteError + " (attempts=3, will retry=false)",
			},
			absent:   []string{"/loc/dirx/a.bin"},
			wantCode: ExitPartial,
		},
		{
			name: "directory remove reports the directory itself too",
			args: []string{"/loc/dir"},
			failures: []vfs.FailedRemoteDelete{
				failedDelete("/loc/dir/b.bin", 1, true),
				failedDelete("/loc/dir", 1, true),
			},
			want: []string{
				"remote delete failed for 2 path(s):",
				"/loc/dir: " + driverDeleteError + " (attempts=1, will retry=true)",
				"/loc/dir/b.bin: " + driverDeleteError + " (attempts=1, will retry=true)",
			},
			wantCode: ExitFailure,
		},
		{
			name:     "idle wait timeout keeps the driver error visible",
			args:     []string{"/loc/a.bin"},
			failures: []vfs.FailedRemoteDelete{failedDelete("/loc/a.bin", 1, true)},
			waitErr:  errors.New("filesystem operations still pending after 30s"),
			want: []string{
				"remote delete failed for /loc/a.bin: " + driverDeleteError + " (attempts=1, will retry=true)",
				"wait:",
				"filesystem operations still pending after 30s",
			},
			wantCode: ExitFailure,
		},
		{
			name:     "interrupted wait keeps its exit status",
			args:     []string{"/loc/a.bin"},
			failures: []vfs.FailedRemoteDelete{failedDelete("/loc/a.bin", 1, true)},
			cancel:   true,
			want: []string{
				"remote delete failed for /loc/a.bin: " + driverDeleteError,
				"context canceled",
			},
			wantCode: ExitInterrupted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, out, configPath := rmTestCommand(t, tt.failures, tt.waitErr)
			if tt.cancel {
				base := shortTempDir(t)
				remote := filepath.Join(base, "remote")
				seedRmTree(t, remote)
				configPath, cfg := writeRmTestConfig(t, base, remote, "loc")
				ctx, ns := startRmNamespace(t, localfs.New(remote), filepath.Join(base, "vfs-storage"), time.Millisecond)
				cmdCtx, cmdCancel := context.WithCancel(context.Background())
				svc := &rmInspectableFS{
					rmNamespaceFS: &rmNamespaceFS{
						ns: ns,
						waitHook: func(waitCtx context.Context, _ string, _ time.Duration) error {
							cmdCancel()
							<-waitCtx.Done()
							return waitCtx.Err()
						},
						failures: tt.failures,
						override: true,
					},
				}
				_ = ctx
				startRmControlServer(t, configPath, cfg, svc)
				cmd = NewRootCommand()
				out = &bytes.Buffer{}
				cmd.SetOut(out)
				cmd.SetErr(out)
				cmd.SetContext(cmdCtx)
				cmd.SetArgs(append([]string{"fs", "--config", configPath, "rm"}, tt.args...))
			} else {
				cmd.SetArgs(append([]string{"fs", "--config", configPath, "rm"}, tt.args...))
			}
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("rm %v succeeded, want a reported delete failure", tt.args)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q missing %q", err.Error(), want)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(err.Error(), absent) {
					t.Fatalf("error %q reported unrelated path %q", err.Error(), absent)
				}
			}
			if got := ExitCode(err); got != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (err=%v)", got, tt.wantCode, err)
			}
			if strings.Contains(out.String(), "removed") {
				t.Fatalf("output claims success: %q", out.String())
			}
		})
	}
}

func TestFsRmPrintsDriverErrorToStderr(t *testing.T) {
	t.Parallel()
	root, out, configPath := rmTestCommand(t, []vfs.FailedRemoteDelete{failedDelete("/loc/a.bin", 1, false)}, nil)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"fs", "--config", configPath, "rm", "/loc/a.bin"})
	err := root.Execute()
	if err == nil {
		t.Fatal("rm succeeded, want a reported delete failure")
	}
	_ = out
	want := "Error: remote delete failed for /loc/a.bin: " + driverDeleteError + " (attempts=1, will retry=false)"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestFsRmSucceedsWithoutRecordedFailure(t *testing.T) {
	t.Parallel()
	root, out, configPath := rmTestCommand(t, nil, nil)
	root.SetArgs([]string{"fs", "--config", configPath, "rm", "/loc/a.bin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("rm without a recorded failure = %v, want success", err)
	}
	if got := out.String(); got != "removed /loc/a.bin\n" {
		t.Fatalf("stdout = %q, want the removed line", got)
	}
}

func TestFsRmJSONReportsRecordedFailure(t *testing.T) {
	t.Parallel()
	root, out, configPath := rmTestCommand(t, []vfs.FailedRemoteDelete{failedDelete("/loc/a.bin", 1, true)}, nil)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"fs", "--config", configPath, "rm", "--json", "/loc/a.bin"})
	err := root.Execute()
	_ = out
	if err == nil {
		t.Fatal("rm --json succeeded, want a reported delete failure")
	}
	if got := ExitCode(err); got != ExitFailure {
		t.Fatalf("exit code = %d, want %d (err=%v)", got, ExitFailure, err)
	}
	var result struct {
		Path    string `json:"path"`
		Removed bool   `json:"removed"`
		Failure []struct {
			Path     string `json:"path"`
			Error    string `json:"error"`
			Attempts int    `json:"attempts"`
			Pending  bool   `json:"pending"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("rm --json output invalid: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if result.Path != "/loc/a.bin" || result.Removed {
		t.Fatalf("rm --json result = %+v, want the path with removed=false", result)
	}
	if len(result.Failure) != 1 {
		t.Fatalf("rm --json failures = %+v, want the recorded failure", result.Failure)
	}
	failure := result.Failure[0]
	if failure.Path != "/loc/a.bin" || failure.Error != driverDeleteError || failure.Attempts != 1 || !failure.Pending {
		t.Fatalf("rm --json failure = %+v, want the driver error, attempts and retry state", failure)
	}
}

func TestFsRmJSONSuccessStaysUnchanged(t *testing.T) {
	t.Parallel()
	root, out, configPath := rmTestCommand(t, nil, nil)
	root.SetArgs([]string{"fs", "--config", configPath, "rm", "--json", "/loc/a.bin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("rm --json = %v, want success", err)
	}
	if !strings.Contains(out.String(), `"removed": true`) {
		t.Fatalf("rm --json output = %s, want removed=true", out.String())
	}
	if strings.Contains(out.String(), "failures") {
		t.Fatalf("rm --json success output carries failures: %s", out.String())
	}
}

// TestFsRmRealVFSPermanentRetryTimeoutDirectoryAndCancel exercises real VFS +
// deletes.jsonl + control CommandServer + fsclient end-to-end without stubbing
// out the VFS delete inspector or task wait loop.
func TestFsRmRealVFSPermanentRetryTimeoutDirectoryAndCancel(t *testing.T) {
	t.Parallel()
	t.Run("permanent remote delete failure", func(t *testing.T) {
		t.Parallel()
		base := shortTempDir(t)
		remote := filepath.Join(base, "remote")
		seedRmTree(t, remote)
		configPath, cfg := writeRmTestConfig(t, base, remote, "loc")
		drv := &rmSpyDriver{
			Driver: localfs.New(remote),
			removeFn: func(context.Context, drive.Entry) error {
				return errors.New(driverDeleteError)
			},
		}
		_, ns := startRmNamespace(t, drv, filepath.Join(base, "vfs-storage"), 10*time.Millisecond)
		startRmControlServer(t, configPath, cfg, &rmInspectableFS{rmNamespaceFS: &rmNamespaceFS{ns: ns}})

		root := NewRootCommand()
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs([]string{"fs", "--config", configPath, "rm", "--json", "/loc/a.bin"})
		err := root.Execute()
		if err == nil || ExitCode(err) != ExitFailure {
			t.Fatalf("err = %v (code=%d), want ExitFailure", err, ExitCode(err))
		}
		if !strings.Contains(stderr.String(), driverDeleteError) || !strings.Contains(stdout.String(), `"removed": false`) {
			t.Fatalf("stdout=%s stderr=%s", stdout.String(), stderr.String())
		}
	})

	t.Run("transient retry timeout", func(t *testing.T) {
		t.Parallel()
		base := shortTempDir(t)
		remote := filepath.Join(base, "remote")
		seedRmTree(t, remote)
		configPath, cfg := writeRmTestConfig(t, base, remote, "loc")
		drv := &rmSpyDriver{
			Driver: localfs.New(remote),
			removeFn: func(context.Context, drive.Entry) error {
				return errors.New("network is unreachable")
			},
		}
		_, ns := startRmNamespace(t, drv, filepath.Join(base, "vfs-storage"), 10*time.Millisecond)
		startRmControlServer(t, configPath, cfg, &rmInspectableFS{rmNamespaceFS: &rmNamespaceFS{ns: ns}})

		root := NewRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"fs", "--config", configPath, "rm", "--wait-timeout", "150ms", "/loc/a.bin"})
		err := root.Execute()
		if err == nil || ExitCode(err) != ExitFailure {
			t.Fatalf("err = %v (code=%d), want ExitFailure", err, ExitCode(err))
		}
		if !strings.Contains(err.Error(), "network is unreachable") || !strings.Contains(err.Error(), "will retry=true") {
			t.Fatalf("err = %v, want transient retry state and driver error", err)
		}
	})

	t.Run("directory delete reports refused directory over control", func(t *testing.T) {
		t.Parallel()
		base := shortTempDir(t)
		remote := filepath.Join(base, "remote")
		seedRmTree(t, remote)
		configPath, cfg := writeRmTestConfig(t, base, remote, "loc")
		baseDrv := localfs.New(remote)
		drv := &rmSpyDriver{
			Driver: baseDrv,
			removeFn: func(ctx context.Context, entry drive.Entry) error {
				if entry.Name == "dir" {
					return errors.New(driverDeleteError)
				}
				return baseDrv.Remove(ctx, entry)
			},
		}
		ctx, ns := startRmNamespace(t, drv, filepath.Join(base, "vfs-storage"), 10*time.Millisecond)
		// Pre-queue child removal before removing /loc/dir (ff43d31).
		if err := ns.Remove(ctx, "/loc/dir/b.bin"); err != nil {
			t.Fatal(err)
		}

		startRmControlServer(t, configPath, cfg, &rmInspectableFS{rmNamespaceFS: &rmNamespaceFS{ns: ns}})
		root := NewRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"fs", "--config", configPath, "rm", "/loc/dir"})
		err := root.Execute()
		if err == nil || ExitCode(err) != ExitFailure {
			t.Fatalf("err = %v (code=%d), want ExitFailure(1)", err, ExitCode(err))
		}
		if !strings.Contains(err.Error(), "/loc/dir") || !strings.Contains(err.Error(), driverDeleteError) {
			t.Fatalf("err = %v, want failure for /loc/dir", err)
		}
	})

	t.Run("shutdown cancelled attempt stays unanswered", func(t *testing.T) {
		t.Parallel()
		base := shortTempDir(t)
		remote := filepath.Join(base, "remote")
		storageDir := filepath.Join(base, "vfs-storage")
		seedRmTree(t, remote)

		inFlight := make(chan struct{}, 1)
		blockingDrv := &rmSpyDriver{
			Driver:   localfs.New(remote),
			activeCh: inFlight,
			removeFn: func(ctx context.Context, _ drive.Entry) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}
		mountFS, err := vfs.New(blockingDrv, vfs.Options{
			StorageDir:  storageDir,
			UploadDelay: time.Hour,
			DeleteDelay: 5 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		ns, err := vfs.NewNamespace([]vfs.Mount{{Name: "loc", FS: mountFS}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		ns.Start(ctx)
		if err := ns.Remove(ctx, "/loc/a.bin"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-inFlight:
		case <-time.After(3 * time.Second):
			t.Fatal("delete attempt did not enter driver Remove")
		}
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := ns.Close(closeCtx); err != nil {
			t.Fatal(err)
		}

		journal, err := idelete.OpenJournal(storageDir)
		if err != nil {
			t.Fatal(err)
		}
		rec, ok := journal.Record("/a.bin")
		_ = journal.Close()
		if !ok || rec.Attempts != 0 || rec.Error != "" {
			t.Fatalf("reopened record = %+v (ok=%v), want unanswered intent with 0 attempts and empty error", rec, ok)
		}

		// Replay on the next owner consumes the full attempt budget cleanly:
		// here the restarted owner completes the remote delete and drains the inspector.
		configPath, cfg := writeRmTestConfig(t, base, remote, "loc")
		_, restartedNS := startRmNamespace(t, localfs.New(remote), storageDir, 5*time.Millisecond)
		endpoint := startRmControlServer(t, configPath, cfg, &rmInspectableFS{rmNamespaceFS: &rmNamespaceFS{ns: restartedNS}})
		client, err := control.NewClient(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		fs := fsclient.New(client)
		deadline := time.Now().Add(3 * time.Second)
		for {
			_, statErr := os.Stat(filepath.Join(remote, "a.bin"))
			if errors.Is(statErr, os.ErrNotExist) && len(fs.FailedRemoteDeletes()) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("replayed delete did not finish on restart: statErr=%v failed=%+v", statErr, fs.FailedRemoteDeletes())
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestFailedRemoteDeletesRequiresDeleteInspector(t *testing.T) {
	t.Parallel()
	var (
		_ vfs.DeleteInspector = (*vfs.VFS)(nil)
		_ vfs.DeleteInspector = (*vfs.Namespace)(nil)
		_ vfs.DeleteInspector = (*fsclient.FS)(nil)
	)
	base := shortTempDir(t)
	remote := filepath.Join(base, "remote")
	seedRmTree(t, remote)
	configPath, cfg := writeRmTestConfig(t, base, remote, "loc")
	_, ns := startRmNamespace(t, localfs.New(remote), filepath.Join(base, "vfs-storage"), time.Millisecond)

	// rmNamespaceFS deliberately omits FSDeleteInspectorService, so /v1/fs/failed-deletes returns 501.
	endpoint := startRmControlServer(t, configPath, cfg, &rmNamespaceFS{ns: ns})
	client, err := control.NewClient(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if _, err := client.FSFailedDeletes(context.Background()); err == nil {
		t.Fatal("FSFailedDeletes without FSDeleteInspectorService succeeded, want HTTP 501 error")
	}
	if got := fsclient.New(client).FailedRemoteDeletes(); got != nil {
		t.Fatalf("fsclient FailedRemoteDeletes without inspector = %+v, want nil", got)
	}

	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"fs", "--config", configPath, "rm", "/loc/a.bin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("fs rm without delete inspector = %v, want success", err)
	}
}
