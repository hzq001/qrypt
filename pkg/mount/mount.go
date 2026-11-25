package mount

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	testclock "github.com/yinzhenyu/qrypt/pkg/clock"
	"github.com/yinzhenyu/qrypt/pkg/logging"
	"github.com/yinzhenyu/qrypt/pkg/vfs"
)

type Options struct {
	MountPoint         string
	ReadOnly           bool
	AllowOther         bool
	DefaultPermissions bool
	VolumeName         string
	PlatformOptions    map[string][]string
	AttrTimeout        time.Duration
	AttrTimeoutSet     bool
	EntryTimeout       time.Duration
	EntryTimeoutSet    bool
	NegativeTimeout    time.Duration
	TotalSpace         int64
	FreeSpace          int64
	Foreground         bool
	ReadyTimeout       time.Duration
	UnmountOnError     bool
	// Clock is the time seam for the mount-readiness timeout. Production
	// leaves it nil, which means the wall clock; a test that exercises the
	// timeout path installs a testclock.Fake and advances it.
	Clock testclock.Clock
}

// clockOrDefault returns the configured clock so production and tests share one
// code path; a nil Clock means the wall clock.
func (o Options) clockOrDefault() testclock.Clock {
	if o.Clock != nil {
		return o.Clock
	}
	return testclock.Real
}

type Session struct {
	ID                       string
	MountPoint               string
	host                     *fuse.FileSystemHost
	adapter                  *adapter
	unsubscribeInvalidations func()

	done    chan struct{}
	endOnce sync.Once
	endErr  error
}

// Done is closed when the underlying FUSE session ends. It covers an external
// unmount, a kernel-initiated teardown such as the macFUSE daemon timeout, and
// a mount failure that happens after Mount has already returned.
func (s *Session) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

// Err reports why the session ended. It is only meaningful after Done is
// closed, and stays nil when the session ended without an error of its own
// (for example the unmount this process requested itself).
func (s *Session) Err() error {
	if s == nil {
		return nil
	}
	return s.endErr
}

// finish publishes the end of the session exactly once. The goroutine that
// runs host.Mount is the only writer of endErr, and it publishes through the
// close of done, so readers that observe Done see the matching endErr.
func (s *Session) finish(err error) {
	if s == nil || s.done == nil {
		// A session without a done channel cannot be observed, so there is
		// nothing to publish.
		return
	}
	s.endOnce.Do(func() {
		s.endErr = err
		close(s.done)
	})
}

// IsMounted reports whether mountPoint is still registered as a mount point.
//
// It separates the two ways a session can end while qrypt is alive. A session
// that dies under a still-registered mount leaves a zombie the process manager
// has to restart (the kernel keeps answering from a mount nobody serves). A
// session whose mount is already gone was unmounted from outside qrypt --
// Finder eject, `diskutil unmount`, `umount` -- which is a normal shutdown and
// must not be reported as a failure, or the manager remounts what the user
// just ejected.
func IsMounted(mountPoint string) bool {
	return mountedAt(mountPoint)
}

type Mounter interface {
	// Mount exports fs at opts.MountPoint until ctx is canceled or the FUSE
	// session ends. It returns a lifecycle handle, or an error for invalid
	// arguments, an unusable mount point, or a failed FUSE startup. The caller
	// retains ownership of fs and must observe Session.Done after startup.
	Mount(ctx context.Context, fs vfs.FileSystem, opts Options) (*Session, error)
	// Unmount stops new operations and detaches session's volume. A nil session
	// is a no-op. ctx bounds waiting for a FSKit helper to release the mount;
	// detach failures and an unfinished helper shutdown are returned as errors.
	Unmount(ctx context.Context, session *Session) error
}

type FuseMounter struct{}

func NewMounter() Mounter {
	return FuseMounter{}
}

func (FuseMounter) Mount(ctx context.Context, fs vfs.FileSystem, opts Options) (*Session, error) {
	if fs == nil {
		return nil, fmt.Errorf("mount: filesystem is nil")
	}
	if opts.MountPoint == "" {
		return nil, fmt.Errorf("mount: mount point required")
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = 5 * time.Second
	}
	// libfuse/macFUSE need the mount point to exist first, but WinFsp owns
	// its mount point directory: FspFileSystemPreflight reports an existing
	// directory as STATUS_OBJECT_NAME_COLLISION ("mount point in use") and
	// creates the directory itself at mount time.
	clock := opts.clockOrDefault()
	if runtime.GOOS != "windows" {
		if err := ensureMountPointWithClock(opts.MountPoint, clock); err != nil {
			return nil, err
		}
	}

	ad := newAdapterWithOptions(fs, adapterOptions{
		Statfs: StatfsOptions{
			TotalSpace: opts.TotalSpace,
			FreeSpace:  opts.FreeSpace,
		},
		ReadOnly:     opts.ReadOnly,
		AllowOther:   opts.AllowOther,
		FlushOnWrite: mountUsesFSKit(opts, runtime.GOOS),
	})
	host := fuse.NewFileSystemHost(ad)
	// qrypt's adapter and VFS synchronize their mutable state internally, so
	// macFUSE may safely run reads of the same vnode concurrently. Without this
	// capability, one slow remote read blocks a seek on another file handle.
	host.SetCapNodeRWLock(true)
	session := &Session{
		ID:         opts.MountPoint,
		MountPoint: opts.MountPoint,
		host:       host,
		adapter:    ad,
		done:       make(chan struct{}),
	}
	session.unsubscribeInvalidations = subscribeInvalidations(fs, host.Notify)

	mountOpts := mountOptions(opts)
	result := make(chan bool, 1)
	go func() {
		ok := host.Mount(opts.MountPoint, mountOpts)
		var endErr error
		if !ok {
			endErr = fmt.Errorf("mount: session for %s stopped serving", opts.MountPoint)
		}
		// The FUSE loop returns when the volume is unmounted or torn down by
		// the kernel. Publish that to the owner so it does not keep running
		// against a dead mount.
		session.finish(endErr)
		result <- ok
	}()
	readyTimer := clock.After(opts.ReadyTimeout)

	select {
	case <-ctx.Done():
		session.unsubscribeInvalidations()
		ad.shutdown()
		host.Unmount()
		return nil, ctx.Err()
	case ok := <-result:
		if !ok {
			session.unsubscribeInvalidations()
			host.Unmount()
			return nil, fmt.Errorf("mount: failed to mount %s", opts.MountPoint)
		}
		return session, nil
	case <-ad.ready:
		return session, nil
	case <-readyTimer:
		// The caller asked to be told when the mount is usable. It is not yet,
		// which usually means the first metadata call into the backend is slow
		// or stuck; keep the session (it may still become ready) but leave a
		// trace so a hung mount is not mistaken for a healthy one.
		logging.L.Warnf("[FUSE] mount not ready after %s mount=%q", opts.ReadyTimeout, opts.MountPoint)
		return session, nil
	}
}

func (FuseMounter) Unmount(ctx context.Context, session *Session) error {
	if session == nil {
		return nil
	}
	start := time.Now()
	logging.L.Infof("[FUSE] unmount start mount=%q", session.MountPoint)
	if session.unsubscribeInvalidations != nil {
		session.unsubscribeInvalidations()
	}
	if session.adapter != nil {
		stepStart := time.Now()
		session.adapter.shutdown()
		logging.L.Infof("[FUSE] adapter shutdown complete mount=%q dur=%s", session.MountPoint, time.Since(stepStart))
	}
	if session.host != nil {
		stepStart := time.Now()
		session.host.Unmount()
		logging.L.Infof("[FUSE] host unmount complete mount=%q dur=%s", session.MountPoint, time.Since(stepStart))
	}
	if cmd := unmountCommand(session.MountPoint); cmd != nil {
		stepStart := time.Now()
		if !mountedAt(session.MountPoint) {
			logging.L.Infof("[FUSE] system unmount skipped mount=%q reason=not mounted", session.MountPoint)
		} else if err := forceUnmount(session.MountPoint); err != nil {
			// The mount is still registered and every access to it will hang.
			// Report it instead of pretending the session ended cleanly: the
			// caller can then exit non-zero and the next start clears the
			// leftover mount before mounting again.
			logging.L.Warnf("[FUSE] system unmount failed mount=%q dur=%s err=%v", session.MountPoint, time.Since(stepStart), err)
			return err
		} else {
			logging.L.Infof("[FUSE] system unmount complete mount=%q dur=%s", session.MountPoint, time.Since(stepStart))
		}
	}
	if session.adapter != nil && session.adapter.flushOnWrite {
		// FUSE-T may keep its FSKit helper alive for several seconds after the
		// volume disappears. It rejects a new mount at the same path until the
		// old helper exits, even though the mount table is already clear.
		if err := waitForFuseTHelperExit(ctx, session.MountPoint); err != nil {
			return err
		}
	}
	logging.L.Infof("[FUSE] unmount complete mount=%q dur=%s", session.MountPoint, time.Since(start))
	return nil
}

func waitForFuseTHelperExit(ctx context.Context, mountPoint string) error {
	ctx, cancel := context.WithTimeout(ctx, detachTimeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for fuseTHelperAliveProbe(mountPoint) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("mount: FUSE-T helper for %s has not stopped: %w", mountPoint, ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
}

func mountOptions(opts Options) []string {
	return mountOptionsForGOOS(opts, runtime.GOOS)
}

func mountUsesFSKit(opts Options, goos string) bool {
	if goos != "darwin" {
		return false
	}
	backend := ""
	for _, option := range opts.PlatformOptions[goos] {
		for part := range strings.SplitSeq(option, ",") {
			if value, ok := strings.CutPrefix(part, "backend="); ok {
				backend = value
			}
		}
	}
	return backend == "fskit"
}

func mountOptionsForGOOS(opts Options, goos string) []string {
	mode := "rw"
	if opts.ReadOnly {
		mode = "ro"
	}
	attrTimeout := opts.AttrTimeout
	if attrTimeout == 0 && !opts.AttrTimeoutSet {
		attrTimeout = time.Second
	}
	entryTimeout := opts.EntryTimeout
	if entryTimeout == 0 && !opts.EntryTimeoutSet {
		entryTimeout = time.Second
	}
	platformOptions := opts.PlatformOptions[goos]
	if opts.PlatformOptions == nil && goos == "darwin" {
		platformOptions = []string{"defer_permissions", "auto_xattr", "iosize=4194304"}
	}
	flags := []string{
		"-o", mode,
		"-o", "attr_timeout=" + fuseTimeout(attrTimeout),
		"-o", "entry_timeout=" + fuseTimeout(entryTimeout),
		"-o", "negative_timeout=" + fuseTimeout(opts.NegativeTimeout),
	}
	// FUSE2 takes -o use_ino so the kernel keeps the inode numbers handed to
	// it by the filesystem; FUSE3 removed the option (it always uses them).
	if fuseUseInoOption != "" {
		flags = append(flags, "-o", fuseUseInoOption)
	}
	if goos == "darwin" {
		flags = append(flags,
			"-o", "fsname=qrypt",
			"-o", "subtype=qrypt",
		)
		// macFUSE defaults daemon_timeout to 60s and ejects the volume when a
		// single request takes longer, which a throttled network read can
		// easily exceed. qrypt owns the cancellation policy; disable the
		// kernel's auto-eject unless the user explicitly configured a limit.
		if !hasPlatformOption(platformOptions, "daemon_timeout") {
			flags = append(flags, "-o", "daemon_timeout=0")
		}
	}
	for _, option := range platformOptions {
		flags = append(flags, "-o", option)
	}
	if goos == "windows" {
		flags = append(flags,
			"-o", "fsname=qrypt",
			// WinFsp enforces POSIX mode bits for access checks. Without
			// uid/gid the caller only gets the "other" bits (typically 0555
			// for qrypt dirs), so every write fails with access denied; -1
			// makes the mounting user the owner of all files.
			"-o", "uid=-1",
			"-o", "gid=-1",
		)
	}
	if opts.AllowOther {
		flags = append(flags, "-o", "allow_other")
	}
	if opts.DefaultPermissions {
		flags = append(flags, "-o", "default_permissions")
	}
	// volname is a macFUSE/WinFsp option; Linux libfuse rejects it with
	// "unknown option", which would make mounting fail on Linux. The
	// volume identity on Linux is carried by fsname/subtype above.
	if opts.VolumeName != "" && goos != "linux" {
		flags = append(flags, "-o", "volname="+opts.VolumeName)
	}
	return flags
}

func hasPlatformOption(options []string, name string) bool {
	prefix := name + "="
	for _, option := range options {
		if option == name || strings.HasPrefix(option, prefix) {
			return true
		}
	}
	return false
}

func fuseTimeout(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	if d%time.Second == 0 {
		return fmt.Sprintf("%d", int64(d/time.Second))
	}
	return fmt.Sprintf("%.3f", d.Seconds())
}
