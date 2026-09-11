package rollout

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// SanitizeAncestors cleans shared fork input too. Ancestors retain their exact
// ordinals and byte offsets; their own writer locks must be available for edits.
func SanitizeAncestors(ctx context.Context, home, threadID, path string) error {
	type ancestor struct{ id, path string }
	ancestors := []ancestor{}
	seen := map[string]bool{threadID: true}
	for depth := 0; ; depth++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth >= 32 {
			return errors.New("fork history exceeds lineage limit")
		}
		base, err := readHistoryBase(home, threadID, path)
		if err != nil {
			return err
		}
		if base == nil {
			break
		}
		if !safeThreadID(base.ThreadID) || seen[base.ThreadID] || base.EndOrdinal == nil || base.EndByte == nil || *base.EndOrdinal < 0 || *base.EndByte < 0 {
			return errors.New("invalid fork history base")
		}
		seen[base.ThreadID] = true
		path, err = locateAncestor(home, base.ThreadID)
		if err != nil {
			return err
		}
		lock := filepath.Join(home, "thread-writer-locks", base.ThreadID+".lock")
		path, err = PreparePlain(ctx, home, base.ThreadID, path, lock)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil || *base.EndByte > info.Size() {
			return errors.New("fork history cutoff exceeds source")
		}
		threadID = base.ThreadID
		ancestors = append(ancestors, ancestor{threadID, path})
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		a := ancestors[i]
		if _, err := SanitizeFile(a.path, filepath.Join(home, "thread-writer-locks", a.id+".lock"), a.id, ContextPolicy{PreserveOffsets: true}); err != nil {
			return err
		}
	}
	return nil
}

type historyBase struct {
	ThreadID   string `json:"thread_id"`
	EndOrdinal *int64 `json:"end_ordinal_exclusive"`
	EndByte    *int64 `json:"end_byte_offset"`
}

func readHistoryBase(home, threadID, path string) (*historyBase, error) {
	if err := ValidatePath(home, threadID, path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid fork metadata file")
	}
	line, err := bufio.NewReaderSize(file, 1<<20).ReadSlice('\n')
	if err != nil {
		return nil, errors.New("invalid or oversized fork metadata")
	}
	if err := validateSessionMeta(line, threadID); err != nil {
		return nil, err
	}
	var meta struct {
		Payload struct {
			Base *historyBase `json:"history_base"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil {
		return nil, errors.New("invalid fork metadata")
	}
	return meta.Payload.Base, nil
}

func locateAncestor(home, threadID string) (string, error) {
	// history_base uses immutable rollout IDs. After revert, those may no longer
	// be the current path of any state-DB row, so resolve the retained file itself.
	patterns := []string{filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl*"), filepath.Join(home, "archived_sessions", "rollout-*-"+threadID+".jsonl*")}
	found := map[string]bool{}
	for _, pattern := range patterns {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			return "", err
		}
		for _, path := range paths {
			if !strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".jsonl.zst") {
				continue
			}
			plain := strings.TrimSuffix(path, ".zst")
			found[plain] = true
		}
	}
	if len(found) != 1 {
		return "", errors.New("missing or ambiguous fork ancestor")
	}
	for path := range found {
		return ResolvePath(home, threadID, path)
	}
	return "", errors.New("missing fork ancestor")
}
