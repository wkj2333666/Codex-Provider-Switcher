// Package rollout contains conservative, atomic repairs for Codex rollout
// JSONL files before they are loaded by app-server.
package rollout

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var (
	// ErrActiveWriter means Codex currently owns the thread writer lock. The
	// caller must leave the rollout untouched and ask the user to reconnect.
	ErrActiveWriter = errors.New("rollout has an active writer")
	ErrUnsafeThread = errors.New("unsafe thread ID")
)

// Fingerprint identifies the durable rollout file without reading its bytes.
// Recovery uses it to skip a sanitation pass that already completed against
// exactly this file version.
func Fingerprint(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("fingerprint rollout")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("unsupported rollout fingerprint")
	}
	return fmt.Sprintf("%d:%d:%d", stat.Ino, info.Size(), stat.Mtim.Nano()), nil
}

// Result describes one sanitation pass.
type Result struct {
	Changed          bool
	ReasoningRemoved int
	IDsStripped      int
}

// ValidatePath accepts only a regular, non-symlink rollout below CODEX_HOME's
// sessions or archived_sessions directory. The path comes from app-server
// thread/read, not from a filename guess or a client-supplied resume template.
func ValidatePath(codexHome, threadID, path string) error {
	if codexHome == "" || !safeThreadID(threadID) || path == "" || !filepath.IsAbs(path) {
		return ErrUnsafeThread
	}
	home, err := filepath.Abs(codexHome)
	if err != nil {
		return errors.New("resolve Codex home")
	}
	if err := validateDirectory(home); err != nil {
		return err
	}
	absPath := filepath.Clean(path)
	for _, rootName := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(home, rootName)
		if err := validateDirectory(root); err != nil {
			continue
		}
		relative, err := filepath.Rel(root, absPath)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		current := root
		safe := true
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			info, statErr := os.Lstat(current)
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
				safe = false
				break
			}
		}
		if !safe {
			continue
		}
		info, err := os.Lstat(absPath)
		if err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
	}
	return errors.New("invalid rollout path")
}

func validateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid rollout directory")
	}
	return nil
}

// SanitizeFile removes provider-bound reasoning records with invalid IDs and
// strips stale item_ IDs from other response items. It never edits the source
// unless every input line is valid JSON and the writer lock is available.
func SanitizeFile(path, lockPath, threadID string) (Result, error) {
	if path == "" {
		return Result{}, errors.New("invalid rollout path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Result{}, fmt.Errorf("inspect rollout: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Result{}, errors.New("invalid rollout file")
	}
	inspection, err := os.Open(path)
	if err != nil {
		return Result{}, errors.New("open rollout")
	}
	preview, inspectErr := scanJSONL(inspection, io.Discard, threadID, true)
	closeErr := inspection.Close()
	if inspectErr != nil {
		return Result{}, inspectErr
	}
	if closeErr != nil {
		return Result{}, errors.New("close rollout")
	}
	if !preview.Changed {
		return preview, nil
	}

	unlock, err := acquireWriterLock(lockPath)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	input, err := os.Open(path)
	if err != nil {
		return Result{}, errors.New("open rollout")
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Dir(path), ".rollout-sanitize-*")
	if err != nil {
		return Result{}, errors.New("create rollout staging file")
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		return Result{}, errors.New("secure rollout staging file")
	}

	result, err := rewriteJSONL(input, temporary, threadID)
	if err != nil {
		return Result{}, err
	}
	if !result.Changed {
		return result, nil
	}
	if err := temporary.Sync(); err != nil {
		return Result{}, errors.New("sync rollout staging file")
	}
	if err := temporary.Close(); err != nil {
		return Result{}, errors.New("close rollout staging file")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return Result{}, errors.New("replace rollout")
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return Result{}, errors.New("open rollout directory")
	}
	syncErr := directory.Sync()
	directoryCloseErr := directory.Close()
	if syncErr != nil || directoryCloseErr != nil {
		return Result{}, errors.New("sync rollout directory")
	}
	committed = true
	return result, nil
}

func rewriteJSONL(input io.Reader, output io.Writer, threadID string) (Result, error) {
	return scanJSONL(input, output, threadID, false)
}

func scanJSONL(input io.Reader, output io.Writer, threadID string, stopOnChange bool) (Result, error) {
	reader := bufio.NewReader(input)
	var result Result
	lineNumber := 0
	paginated := false
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) != 0 {
			lineNumber++
			if lineNumber == 1 {
				var record struct {
					Ordinal *uint64 `json:"ordinal"`
					Payload struct {
						HistoryMode string `json:"history_mode"`
					} `json:"payload"`
				}
				if err := json.Unmarshal(line, &record); err != nil {
					return Result{}, errors.New("invalid rollout JSONL")
				}
				paginated = record.Payload.HistoryMode == "paginated" || record.Ordinal != nil
			}
			cleaned, keep, changed, removed, stripped, parseErr := sanitizeLine(line, paginated)
			if parseErr != nil {
				return Result{}, errors.New("invalid rollout JSONL")
			}
			if lineNumber == 1 && threadID != "" {
				if err := validateSessionMeta(cleaned, threadID); err != nil {
					return Result{}, err
				}
			}
			if keep {
				if _, writeErr := output.Write(cleaned); writeErr != nil {
					return Result{}, errors.New("write rollout staging file")
				}
			}
			result.Changed = result.Changed || changed
			result.ReasoningRemoved += removed
			result.IDsStripped += stripped
			// A preliminary scan only decides whether to acquire the writer
			// lock. The rewrite under that lock still validates every record
			// before atomically replacing the original file.
			if stopOnChange && result.Changed {
				return result, nil
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, errors.New("read rollout")
		}
	}
	return result, nil
}

func validateSessionMeta(line []byte, threadID string) error {
	var envelope struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &envelope) != nil ||
		envelope.Type != "session_meta" || envelope.Payload.ID != threadID {
		return errors.New("rollout session metadata does not match thread")
	}
	return nil
}

func sanitizeLine(line []byte, paginated bool) ([]byte, bool, bool, int, int, error) {
	trimmed := bytes.TrimSuffix(line, []byte{'\n'})
	if len(bytes.TrimSpace(trimmed)) == 0 {
		return nil, false, false, 0, 0, errors.New("empty rollout line")
	}
	// Parse only the small envelope first. A large rollout contains many
	// event records; decoding every payload into maps made recovery scans
	// unnecessarily expensive.
	var probe struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, false, false, 0, 0, err
	}
	if probe.Type != "response_item" && probe.Type != "compacted" {
		return line, true, false, 0, 0, nil
	}
	if probe.Type == "response_item" {
		if !bytes.Contains(probe.Payload, []byte(`"id":`)) {
			return line, true, false, 0, 0, nil
		}
	} else if !bytes.Contains(probe.Payload, []byte(`"replacement_history"`)) ||
		!bytes.Contains(probe.Payload, []byte(`"id":`)) {
		return line, true, false, 0, 0, nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, false, false, 0, 0, err
	}
	var envelopeType string
	if rawType, present := envelope["type"]; present {
		if err := json.Unmarshal(rawType, &envelopeType); err != nil {
			return nil, false, false, 0, 0, err
		}
	}
	removed, stripped := 0, 0
	if envelopeType == "compacted" {
		var compact map[string]json.RawMessage
		if err := json.Unmarshal(envelope["payload"], &compact); err != nil {
			return nil, false, false, 0, 0, err
		}
		raw, exists := compact["replacement_history"]
		if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return line, true, false, 0, 0, nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, false, false, 0, 0, err
		}
		clean := make([]json.RawMessage, 0, len(items))
		for _, item := range items {
			encoded, drop, changed, err := sanitizeResponseItem(item)
			if err != nil {
				return nil, false, false, 0, 0, err
			}
			if drop {
				removed++
				continue
			}
			if changed {
				stripped++
			}
			clean = append(clean, encoded)
		}
		if removed+stripped == 0 {
			return line, true, false, 0, 0, nil
		}
		var err error
		compact["replacement_history"], err = marshalUnescaped(clean)
		if err != nil {
			return nil, false, false, 0, 0, err
		}
		envelope["payload"], err = marshalUnescaped(compact)
		if err != nil {
			return nil, false, false, 0, 0, err
		}
	} else {
		encoded, drop, changed, err := sanitizeResponseItem(envelope["payload"])
		if err != nil {
			return nil, false, false, 0, 0, err
		}
		if !changed {
			return line, true, false, 0, 0, nil
		}
		if drop {
			if _, numbered := envelope["ordinal"]; paginated && !numbered {
				return nil, false, false, 0, 0, errors.New("paginated rollout has missing ordinal")
			}
			// Codex discards ResponseItem::Other from model context. Keep a
			// numbered no-op instead of shifting every downstream ordinal.
			encoded = json.RawMessage(`{"type":"other"}`)
			removed = 1
		} else {
			stripped = 1
		}
		envelope["payload"] = encoded
	}
	encoded, err := marshalUnescaped(envelope)
	if err != nil {
		return nil, false, false, 0, 0, err
	}
	if raw, numbered := envelope["ordinal"]; numbered {
		var ordinal uint64
		if err := json.Unmarshal(raw, &ordinal); err != nil || bytes.Equal(raw, []byte("null")) {
			return nil, false, false, 0, 0, errors.New("invalid rollout ordinal")
		}
		// SQLite page cursors and fork history_base reference absolute bytes.
		// JSON trailing whitespace preserves them without modifying the DB.
		if len(encoded) > len(trimmed) {
			return nil, false, false, 0, 0, errors.New("sanitation would shift rollout offsets")
		}
		encoded = append(encoded, bytes.Repeat([]byte{' '}, len(trimmed)-len(encoded))...)
	}
	if bytes.HasSuffix(line, []byte{'\n'}) {
		encoded = append(encoded, '\n')
	}
	return encoded, true, true, removed, stripped, nil
}

func marshalUnescaped(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

// The UI's item_completed events deliberately remain unchanged: they do not
// feed model context, and their IDs are valid keys in the history projection.
func sanitizeResponseItem(raw json.RawMessage) (json.RawMessage, bool, bool, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false, false, err
	}
	var itemType, id string
	if rawType, present := payload["type"]; present {
		if err := json.Unmarshal(rawType, &itemType); err != nil {
			return nil, false, false, err
		}
	} else {
		return nil, false, false, errors.New("response item has no type")
	}
	if rawID, present := payload["id"]; present && !bytes.Equal(bytes.TrimSpace(rawID), []byte("null")) {
		if err := json.Unmarshal(rawID, &id); err != nil {
			return nil, false, false, err
		}
	}
	if id == "" || !strings.HasPrefix(id, "item_") {
		return raw, false, false, nil
	}
	if itemType == "reasoning" {
		return nil, true, true, nil
	}
	delete(payload, "id")
	encoded, err := marshalUnescaped(payload)
	return encoded, false, true, err
}

func acquireWriterLock(path string) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	directory := filepath.Dir(path)
	if err := ensureLockDirectory(directory); err != nil {
		return nil, err
	}
	coordinationPath := filepath.Join(directory, ".coordination.lock")
	coordination, err := openLockFile(coordinationPath)
	if err != nil {
		return nil, errors.New("open thread writer coordination lock")
	}
	if err := syscall.Flock(int(coordination.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = coordination.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrActiveWriter
		}
		return nil, errors.New("inspect thread writer coordination lock")
	}
	file, err := openLockFile(path)
	if err != nil {
		_ = syscall.Flock(int(coordination.Fd()), syscall.LOCK_UN)
		_ = coordination.Close()
		return nil, errors.New("open thread writer lock")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		_ = syscall.Flock(int(coordination.Fd()), syscall.LOCK_UN)
		_ = coordination.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrActiveWriter
		}
		return nil, errors.New("inspect thread writer lock")
	}
	_ = syscall.Flock(int(coordination.Fd()), syscall.LOCK_UN)
	_ = coordination.Close()
	return func() {
		coordination, coordinationErr := openLockFile(coordinationPath)
		if coordinationErr == nil {
			if syscall.Flock(int(coordination.Fd()), syscall.LOCK_EX) == nil {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
				_ = os.Remove(path)
				_ = syscall.Flock(int(coordination.Fd()), syscall.LOCK_UN)
				_ = coordination.Close()
				return
			}
			_ = coordination.Close()
		}
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func ensureLockDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return errors.New("create thread writer lock directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid thread writer lock directory")
	}
	return nil
}

func openLockFile(path string) (*os.File, error) {
	for attempts := 0; attempts < 3; attempts++ {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if errors.Is(createErr, os.ErrExist) {
				continue
			}
			if createErr != nil {
				return nil, errors.New("create thread writer lock")
			}
			return file, nil
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("invalid thread writer lock")
		}
		file, openErr := os.OpenFile(path, os.O_RDWR, 0)
		if openErr != nil {
			return nil, errors.New("open thread writer lock")
		}
		openedInfo, statErr := file.Stat()
		if statErr == nil && os.SameFile(info, openedInfo) {
			return file, nil
		}
		_ = file.Close()
	}
	return nil, errors.New("thread writer lock changed during inspection")
}

func safeThreadID(threadID string) bool {
	if threadID == "" || len(threadID) > 256 {
		return false
	}
	for _, character := range threadID {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}
