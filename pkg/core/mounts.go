package core

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/control"
	"github.com/yinzhenyu/qrypt/pkg/logging"
	"github.com/yinzhenyu/qrypt/pkg/vfs"
	"github.com/yinzhenyu/qrypt/pkg/vfs/subtree"
)

// MountRequest asks a Core to host one FUSE session for a configured view.
type MountRequest struct {
	// Name selects a configured mount subtree (/NAME); empty means the whole
	// namespace root.
	Name       string `json:"name,omitempty"`
	MountPoint string `json:"mount_point"`
	ReadOnly   bool   `json:"read_only,omitempty"`
}

// HostedMount is the observable state of one hosted FUSE session.
type HostedMount struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	MountPoint string    `json:"mount_point"`
	ReadOnly   bool      `json:"read_only,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

// MountSession is a FUSE session whose kernel callbacks are served by this
// process. Done is closed when the session ends (external unmount, kernel
// teardown or a failed mount after Mount returned).
type MountSession interface {
	ID() string
	MountPoint() string
	Done() <-chan struct{}
}

// MountSessionState is an optional extension of MountSession that reports why a
// finished session stopped and whether the OS mount table still lists its mount
// point. pkg/core reads it so the exit-reason bridge needs no pkg/mount import.
type MountSessionState interface {
	Err() error
	IsMounted() bool
}

// MountEndReason classifies why a hosted FUSE session left the active table.
type MountEndReason string

const (
	MountEndDetached   MountEndReason = "detached"
	MountEndExternal   MountEndReason = "external"
	MountEndUnexpected MountEndReason = "unexpected"
)

// EndedMount records the outcome of a finished hosted FUSE session.
type EndedMount struct {
	ID         string         `json:"id"`
	MountPoint string         `json:"mount_point"`
	Reason     MountEndReason `json:"reason"`
	Error      string         `json:"error,omitempty"`
	EndedAt    time.Time      `json:"ended_at"`
}

const maxEndedMounts = 64

// MountHost mounts a filesystem view on the host. The desktop composition root
// supplies the FUSE implementation. pkg/core must not import pkg/mount: that
// would drag cgofuse into embedded builds such as the gomobile library.
type MountHost interface {
	Mount(ctx context.Context, fs vfs.FileSystem, req MountRequest) (MountSession, error)
	Unmount(ctx context.Context, session MountSession) error
}

type mountEntry struct {
	req     MountRequest
	session MountSession
	info    HostedMount
}

// mountManager owns every FUSE session hosted by a Core. All mutations of the
// registry happen under mu; Mount/Unmount calls happen outside it so a slow
// mount never blocks listing.
type mountManager struct {
	mu         sync.Mutex
	host       MountHost
	mounts     map[string]*mountEntry
	ended      map[string]EndedMount
	endedOrder []string
	closed     bool
	seq        uint64
}

func newMountManager(host MountHost) *mountManager {
	if host == nil {
		return nil
	}
	return &mountManager{
		host:   host,
		mounts: map[string]*mountEntry{},
		ended:  map[string]EndedMount{},
	}
}

func (m *mountManager) attach(ctx context.Context, fs vfs.FileSystem, req MountRequest) (HostedMount, error) {
	if m == nil {
		return HostedMount{}, fmt.Errorf("core: mounting is not enabled for this runtime")
	}
	req.MountPoint = strings.TrimSpace(req.MountPoint)
	if req.MountPoint == "" || !filepath.IsAbs(req.MountPoint) {
		return HostedMount{}, fmt.Errorf("core: mount point must be an absolute path")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return HostedMount{}, fmt.Errorf("core: closed")
	}
	for _, entry := range m.mounts {
		if entry.info.MountPoint == req.MountPoint {
			m.mu.Unlock()
			return HostedMount{}, fmt.Errorf("core: %s is already mounted", req.MountPoint)
		}
	}
	m.mu.Unlock()

	// Mount outside the lock; a second attach may race us for the same mount
	// point and is rejected after the fact below.
	view, err := subtree.New(fs, req.Name)
	if err != nil {
		return HostedMount{}, err
	}
	session, err := m.host.Mount(ctx, view, req)
	if err != nil {
		return HostedMount{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = m.host.Unmount(context.Background(), session)
		return HostedMount{}, fmt.Errorf("core: closed")
	}
	for _, entry := range m.mounts {
		if entry.info.MountPoint == req.MountPoint {
			m.mu.Unlock()
			_ = m.host.Unmount(context.Background(), session)
			return HostedMount{}, fmt.Errorf("core: %s is already mounted", req.MountPoint)
		}
	}
	m.seq++
	id := session.ID()
	if id == "" {
		id = fmt.Sprintf("mount-%d", m.seq)
	}
	info := HostedMount{
		ID:         id,
		Name:       req.Name,
		MountPoint: req.MountPoint,
		ReadOnly:   req.ReadOnly,
		StartedAt:  time.Now().UTC(),
	}
	m.clearEndedLocked(id)
	m.mounts[id] = &mountEntry{req: req, session: session, info: info}
	m.mu.Unlock()

	go m.watch(id, session)
	return info, nil
}

func (m *mountManager) clearEndedLocked(id string) {
	if _, ok := m.ended[id]; !ok {
		return
	}
	delete(m.ended, id)
	for i, existing := range m.endedOrder {
		if existing == id {
			m.endedOrder = append(m.endedOrder[:i], m.endedOrder[i+1:]...)
			break
		}
	}
}

func (m *mountManager) recordEndedLocked(outcome EndedMount) {
	if m.ended == nil {
		m.ended = map[string]EndedMount{}
	}
	m.clearEndedLocked(outcome.ID)
	m.ended[outcome.ID] = outcome
	m.endedOrder = append(m.endedOrder, outcome.ID)
	for len(m.endedOrder) > maxEndedMounts {
		oldest := m.endedOrder[0]
		m.endedOrder = m.endedOrder[1:]
		delete(m.ended, oldest)
	}
}

// watch removes an entry when its FUSE session ends outside detach/closeAll and
// classifies whether the volume was unmounted externally or died unexpectedly.
func (m *mountManager) watch(id string, session MountSession) {
	done := session.Done()
	if done == nil {
		return
	}
	<-done

	m.mu.Lock()
	entry, ok := m.mounts[id]
	if !ok || entry.session != session {
		m.mu.Unlock()
		return
	}
	mountPoint := entry.info.MountPoint
	m.mu.Unlock()

	reason := MountEndUnexpected
	errText := "FUSE session ended unexpectedly"
	if state, hasState := session.(MountSessionState); hasState {
		if !state.IsMounted() {
			reason = MountEndExternal
			errText = ""
		} else if err := state.Err(); err != nil {
			errText = err.Error()
		}
	}

	m.mu.Lock()
	if current, stillActive := m.mounts[id]; stillActive && current.session == session {
		delete(m.mounts, id)
		m.recordEndedLocked(EndedMount{
			ID:         id,
			MountPoint: mountPoint,
			Reason:     reason,
			Error:      errText,
			EndedAt:    time.Now().UTC(),
		})
	}
	m.mu.Unlock()
}

func (m *mountManager) detach(ctx context.Context, id string) error {
	if m == nil {
		return fmt.Errorf("core: mounting is not enabled for this runtime")
	}
	m.mu.Lock()
	entry, ok := m.mounts[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("core: mount %q not found", id)
	}
	delete(m.mounts, id)
	// Record detached before Unmount closes session.Done so watch cannot
	// overwrite an intentional detach with an external or unexpected outcome.
	m.recordEndedLocked(EndedMount{
		ID:         id,
		MountPoint: entry.info.MountPoint,
		Reason:     MountEndDetached,
		EndedAt:    time.Now().UTC(),
	})
	m.mu.Unlock()
	return m.host.Unmount(ctx, entry.session)
}

func (m *mountManager) list() []HostedMount {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	infos := make([]HostedMount, 0, len(m.mounts))
	for _, entry := range m.mounts {
		infos = append(infos, entry.info)
	}
	m.mu.Unlock()
	sort.Slice(infos, func(i, j int) bool { return infos[i].MountPoint < infos[j].MountPoint })
	return infos
}

func (m *mountManager) closeAll(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	entries := make([]*mountEntry, 0, len(m.mounts))
	now := time.Now().UTC()
	for id, entry := range m.mounts {
		entries = append(entries, entry)
		m.recordEndedLocked(EndedMount{
			ID:         id,
			MountPoint: entry.info.MountPoint,
			Reason:     MountEndDetached,
			EndedAt:    now,
		})
	}
	m.mounts = map[string]*mountEntry{}
	m.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
	}
	for _, entry := range entries {
		if err := m.host.Unmount(ctx, entry.session); err != nil {
			logging.L.Warnf("[CORE] detach mount %s: %v", entry.info.MountPoint, err)
		}
	}
}

// AttachMount mounts a view of the core filesystem through the injected
// MountHost. A named view requires the core to serve a namespace; embedded
// single-mount runtimes only expose their root.
func (c *Core) AttachMount(ctx context.Context, req MountRequest) (HostedMount, error) {
	if c == nil || c.fs == nil {
		return HostedMount{}, fmt.Errorf("core: closed")
	}
	if req.Name != "" {
		if _, ok := c.mountNames[req.Name]; !ok {
			return HostedMount{}, fmt.Errorf("core: mount %q not found", req.Name)
		}
		if _, ok := c.fs.(*vfs.Namespace); !ok {
			return HostedMount{}, fmt.Errorf("core: this runtime serves a single mount and cannot host subtree views")
		}
	}
	return c.mounts.attach(ctx, c.fs, req)
}

// DetachMount unmounts one hosted FUSE session by id (its mount point).
func (c *Core) DetachMount(ctx context.Context, id string) error {
	if c == nil {
		return fmt.Errorf("core: closed")
	}
	return c.mounts.detach(ctx, id)
}

func (m *mountManager) endedOutcome(id string) (EndedMount, bool) {
	if m == nil {
		return EndedMount{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ended, ok := m.ended[id]
	return ended, ok
}

func (m *mountManager) controlOutcome(id string) (control.MountOutcome, bool) {
	if m == nil {
		return control.MountOutcome{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.mounts[id]; ok {
		return control.MountOutcome{
			ID:     entry.info.ID,
			Active: true,
		}, true
	}
	if ended, ok := m.ended[id]; ok {
		return control.MountOutcome{
			ID:     ended.ID,
			Active: false,
			Reason: string(ended.Reason),
			Error:  ended.Error,
		}, true
	}
	return control.MountOutcome{}, false
}

// MountOutcome returns the recorded end state for a finished hosted mount, if
// known. Active mounts and evicted entries return false.
func (c *Core) MountOutcome(id string) (EndedMount, bool) {
	if c == nil {
		return EndedMount{}, false
	}
	return c.mounts.endedOutcome(id)
}

// ListMounts returns a snapshot of the hosted FUSE sessions.
func (c *Core) ListMounts() []HostedMount {
	if c == nil {
		return nil
	}
	return c.mounts.list()
}

func configuredMountNames(cfgMounts []string) map[string]struct{} {
	names := make(map[string]struct{}, len(cfgMounts))
	for _, name := range cfgMounts {
		names[name] = struct{}{}
	}
	return names
}

// coreMountService adapts the mount manager to the transport-facing contract.
type coreMountService struct {
	core *Core
}

func (s coreMountService) ListMounts(context.Context) ([]control.MountStatus, error) {
	infos := s.core.ListMounts()
	statuses := make([]control.MountStatus, 0, len(infos))
	for _, info := range infos {
		statuses = append(statuses, control.MountStatus{
			ID:         info.ID,
			Name:       info.Name,
			MountPoint: info.MountPoint,
			ReadOnly:   info.ReadOnly,
			StartedAt:  info.StartedAt,
		})
	}
	return statuses, nil
}

func (s coreMountService) AttachMount(ctx context.Context, req control.MountAttachRequest) (control.MountStatus, error) {
	info, err := s.core.AttachMount(ctx, MountRequest{Name: req.Name, MountPoint: req.MountPoint, ReadOnly: req.ReadOnly})
	if err != nil {
		return control.MountStatus{}, err
	}
	return control.MountStatus{
		ID:         info.ID,
		Name:       info.Name,
		MountPoint: info.MountPoint,
		ReadOnly:   info.ReadOnly,
		StartedAt:  info.StartedAt,
	}, nil
}

func (s coreMountService) DetachMount(ctx context.Context, id string) error {
	return s.core.DetachMount(ctx, id)
}

func (s coreMountService) MountOutcome(_ context.Context, id string) (control.MountOutcome, error) {
	if s.core == nil || s.core.mounts == nil {
		return control.MountOutcome{}, fmt.Errorf("core: mount %q not found", id)
	}
	outcome, ok := s.core.mounts.controlOutcome(id)
	if !ok {
		return control.MountOutcome{}, fmt.Errorf("core: mount %q not found", id)
	}
	return outcome, nil
}
