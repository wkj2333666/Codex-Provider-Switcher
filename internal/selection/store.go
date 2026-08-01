// Package selection persists the selected provider for each Codex task.
package selection

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/provider"
)

const maximumStateSize = 256

// Store keeps private per-task provider selections in one directory.
type Store struct {
	directory string
}

// Open creates or validates a private state directory.
func Open(directory string) (*Store, error) {
	if directory == "" {
		return nil, errors.New("provider selection state directory is required")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, errors.New("resolve provider selection state directory")
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(absolute, 0o700); err != nil {
			return nil, errors.New("create provider selection state directory")
		}
		info, err = os.Lstat(absolute)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("invalid provider selection state directory")
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, errors.New("secure provider selection state directory")
	}
	return &Store{directory: absolute}, nil
}

// Get returns a task's durable provider selection when one exists.
func (store *Store) Get(threadID string) (string, bool, error) {
	if store == nil || threadID == "" {
		return "", false, errors.New("invalid provider selection lookup")
	}
	file, err := os.Open(store.path(threadID))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errors.New("open provider selection")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximumStateSize {
		return "", false, errors.New("invalid provider selection state")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumStateSize+1))
	if err != nil || len(data) > maximumStateSize {
		return "", false, errors.New("read provider selection")
	}
	value := string(bytes.TrimSuffix(data, []byte{'\n'}))
	if !provider.Valid(value) {
		return "", false, errors.New("invalid provider selection state")
	}
	return value, true, nil
}

// Set atomically replaces a task's durable provider selection.
func (store *Store) Set(threadID, value string) error {
	if store == nil || threadID == "" || !provider.Valid(value) {
		return errors.New("invalid provider selection")
	}
	temporary, err := os.CreateTemp(store.directory, ".provider-*.tmp")
	if err != nil {
		return errors.New("create provider selection state")
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return errors.New("secure provider selection state")
	}
	if _, err := temporary.Write(append([]byte(value), '\n')); err != nil {
		cleanup()
		return errors.New("write provider selection state")
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return errors.New("sync provider selection state")
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return errors.New("close provider selection state")
	}
	if err := os.Rename(temporaryPath, store.path(threadID)); err != nil {
		_ = os.Remove(temporaryPath)
		return errors.New("replace provider selection state")
	}
	directory, err := os.Open(store.directory)
	if err != nil {
		return errors.New("open provider selection state directory")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("sync provider selection state directory")
	}
	return nil
}

func (store *Store) path(threadID string) string {
	digest := sha256.Sum256([]byte(threadID))
	return filepath.Join(store.directory, "provider-"+hex.EncodeToString(digest[:16])+".state")
}
