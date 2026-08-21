// Package selection persists the selected provider for each Codex task.
package selection

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/provider"
)

const maximumStateSize = 256

// Store keeps private per-task provider selections in one directory.
type Store struct {
	directory string
}

// Value is the durable provider route selected for one task.
type Value struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
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
	value, ok, err := store.GetRoute(threadID)
	return value.Provider, ok, err
}

// GetRoute returns a task's durable provider/model selection when one exists.
// Legacy provider-only state remains readable.
func (store *Store) GetRoute(threadID string) (Value, bool, error) {
	if store == nil || threadID == "" {
		return Value{}, false, errors.New("invalid provider selection lookup")
	}
	file, err := os.Open(store.path(threadID))
	if errors.Is(err, os.ErrNotExist) {
		return Value{}, false, nil
	}
	if err != nil {
		return Value{}, false, errors.New("open provider selection")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximumStateSize {
		return Value{}, false, errors.New("invalid provider selection state")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumStateSize+1))
	if err != nil || len(data) > maximumStateSize {
		return Value{}, false, errors.New("read provider selection")
	}
	legacy := bytes.TrimSuffix(data, []byte{'\n'})
	if provider.Valid(string(legacy)) {
		return Value{Provider: string(legacy)}, true, nil
	}
	value, err := decodeValue(data)
	if err != nil {
		return Value{}, false, errors.New("invalid provider selection state")
	}
	return value, true, nil
}

// Set atomically replaces a task's durable provider selection.
func (store *Store) Set(threadID, value string) error {
	return store.SetRoute(threadID, Value{Provider: value})
}

// SetRoute atomically replaces a task's durable provider/model selection.
func (store *Store) SetRoute(threadID string, value Value) error {
	if store == nil || threadID == "" || !validValue(value) {
		return errors.New("invalid provider selection")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("encode provider selection")
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
	if _, err := temporary.Write(append(data, '\n')); err != nil {
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

func decodeValue(data []byte) (Value, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return Value{}, errors.New("invalid selection")
	}
	var value Value
	seen := make(map[string]struct{})
	for decoder.More() {
		rawKey, err := decoder.Token()
		key, ok := rawKey.(string)
		if err != nil || !ok {
			return Value{}, errors.New("invalid selection")
		}
		if _, duplicate := seen[key]; duplicate {
			return Value{}, errors.New("invalid selection")
		}
		seen[key] = struct{}{}
		switch key {
		case "provider":
			if decoder.Decode(&value.Provider) != nil {
				return Value{}, errors.New("invalid selection")
			}
		case "model":
			if decoder.Decode(&value.Model) != nil {
				return Value{}, errors.New("invalid selection")
			}
		default:
			return Value{}, errors.New("invalid selection")
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF || !validValue(value) {
		return Value{}, errors.New("invalid selection")
	}
	return value, nil
}

func validValue(value Value) bool {
	return provider.Valid(value.Provider) && (value.Model == "" || modelroute.ValidModel(value.Model))
}

func (store *Store) path(threadID string) string {
	digest := sha256.Sum256([]byte(threadID))
	return filepath.Join(store.directory, "provider-"+hex.EncodeToString(digest[:16])+".state")
}
