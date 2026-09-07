package rollout

import (
	"os"
	"path/filepath"
	"syscall"
)

// NoteAccess conservatively delays cold-history compression for any task RPC,
// including read-only requests that do not update the rollout's mtime.
func NoteAccess(home, threadID string) {
	if home == "" || !safeThreadID(threadID) {
		return
	}
	dir := filepath.Join(home, "codex-provider-switcher", "history-access")
	if ensureLockDirectory(dir) != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(dir, threadID), os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	_, _ = file.WriteAt([]byte("\n"), 0)
}
