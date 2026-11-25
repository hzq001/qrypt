package control

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yinzhenyu/qrypt/pkg/logging"
	"github.com/yinzhenyu/qrypt/pkg/vfs/diagnostics"

	"github.com/yinzhenyu/qrypt/pkg/vfs"
)

func (s *Server) debugSnapshot(r *http.Request) diagnostics.DebugSnapshot {
	mounts := debugMountQuery(r)
	if len(mounts) == 0 {
		return s.source.DebugSnapshot()
	}
	if filtered, ok := s.source.(diagnostics.DebugMountSnapshotter); ok {
		return filtered.DebugSnapshotForMounts(mounts)
	}
	return filterDebugSnapshotMounts(s.source.DebugSnapshot(), mounts)
}

func debugMountQuery(r *http.Request) []string {
	var out []string
	for _, name := range r.URL.Query()["mount"] {
		name = strings.TrimSpace(name)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func filterDebugSnapshotMounts(snapshot diagnostics.DebugSnapshot, mountNames []string) diagnostics.DebugSnapshot {
	if len(mountNames) == 0 {
		return snapshot
	}
	allowed := debugMountSet(mountNames)
	filtered := snapshot.Mounts[:0]
	for _, mount := range snapshot.Mounts {
		if allowed[mount.Identity.Name] {
			filtered = append(filtered, mount)
		}
	}
	snapshot.Mounts = filtered
	return snapshot
}

func debugMountSet(mountNames []string) map[string]bool {
	set := map[string]bool{}
	for _, name := range mountNames {
		name = strings.Trim(strings.TrimSpace(name), "/")
		if name != "" {
			set[name] = true
		}
	}
	return set
}

func debugMountAllowed(mountName string, mountNames []string) bool {
	if len(mountNames) == 0 {
		return true
	}
	return debugMountSet(mountNames)[mountName]
}

func writeJSON(w http.ResponseWriter, value any) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(append(body, '\n')); err != nil {
		// Write commits the status even when the connection fails. A second
		// response cannot report that failure to the disconnected client.
		logging.L.Warnf("[CONTROL] JSON response write failed: %v", err)
	}
}

func joinVirtual(parent, child string) string {
	if child == "" || child == "/" {
		return parent
	}
	if child[0] == '/' {
		child = child[1:]
	}
	if parent == "/" {
		return "/" + child
	}
	return parent + "/" + child
}

func cleanVirtual(path string) string {
	return vfs.CleanVirtualPath(path)
}

func parseBoolQuery(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
