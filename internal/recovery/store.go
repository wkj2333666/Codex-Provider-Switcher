// Package recovery persists crash-safe provider recovery transactions.
package recovery

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
	providerid "github.com/wkj2333666/Codex-Provider-Switcher/internal/provider"
)

const (
	maxJournalSize = 16 << 10
	maxThreadIDs   = 64
)

// Journal is the minimal durable state needed to repair an interrupted reload.
type Journal struct {
	Version   int      `json:"version"`
	RootID    string   `json:"rootId"`
	Provider  string   `json:"provider"`
	Model     string   `json:"model,omitempty"`
	Phase     string   `json:"phase"`
	Threads   []string `json:"threads"`
	Remaining []string `json:"remaining"`
}

// Store owns recovery journals under one provider selection directory.
type Store struct {
	directory string
}

// Open creates or validates the private recovery directory.
func Open(stateDirectory string) (*Store, error) {
	if stateDirectory == "" {
		return nil, errors.New("invalid recovery state directory")
	}
	directory := filepath.Join(stateDirectory, "recovery")
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, errors.New("create recovery state directory")
		}
		info, err = os.Lstat(directory)
	}
	if err != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("invalid recovery state directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, errors.New("secure recovery state directory")
	}
	return &Store{directory: directory}, nil
}

// Load returns a validated journal for one root thread.
func (store *Store) Load(rootID string) (Journal, bool, error) {
	if store == nil || rootID == "" {
		return Journal{}, false, errors.New("invalid recovery journal key")
	}
	path := store.path(rootID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Journal{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return Journal{}, false, errors.New("invalid recovery journal file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Journal{}, false, errors.New("open recovery journal")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJournalSize+1))
	if err != nil || len(data) > maxJournalSize {
		return Journal{}, false, errors.New("read recovery journal")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var journal Journal
	if decoder.Decode(&journal) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		validateJournal(journal) != nil || journal.RootID != rootID {
		return Journal{}, false, errors.New("invalid recovery journal")
	}
	return journal, true, nil
}

// Save atomically replaces one validated recovery journal.
func (store *Store) Save(journal Journal) error {
	if store == nil || validateJournal(journal) != nil {
		return errors.New("invalid recovery journal")
	}
	encoded, err := json.Marshal(journal)
	if err != nil || len(encoded) > maxJournalSize {
		return errors.New("encode recovery journal")
	}
	temporary, err := os.CreateTemp(store.directory, ".journal-*")
	if err != nil {
		return errors.New("create recovery journal")
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.New("secure recovery journal")
	}
	if _, err := temporary.Write(encoded); err != nil {
		return errors.New("write recovery journal")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("write recovery journal")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("write recovery journal")
	}
	if err := os.Rename(temporaryPath, store.path(journal.RootID)); err != nil {
		return errors.New("replace recovery journal")
	}
	directory, err := os.Open(store.directory)
	if err != nil {
		return errors.New("open recovery state directory")
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("sync recovery state directory")
	}
	committed = true
	return nil
}

// Clear removes a completed journal and syncs its directory.
func (store *Store) Clear(rootID string) error {
	if store == nil || rootID == "" {
		return errors.New("invalid recovery journal key")
	}
	if err := os.Remove(store.path(rootID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("clear recovery journal")
	}
	directory, err := os.Open(store.directory)
	if err != nil {
		return errors.New("open recovery state directory")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("sync recovery state directory")
	}
	return nil
}

func (store *Store) path(rootID string) string {
	digest := sha256.Sum256([]byte(rootID))
	return filepath.Join(store.directory, hex.EncodeToString(digest[:16])+".json")
}

func validateJournal(journal Journal) error {
	validRouteVersion := (journal.Version == 1 && journal.Model == "") ||
		(journal.Version == 2 && modelroute.ValidModel(journal.Model))
	if !validRouteVersion || journal.RootID == "" || len(journal.RootID) > 1024 ||
		!providerid.Valid(journal.Provider) ||
		(journal.Phase != "prepared" && journal.Phase != "restoring") ||
		len(journal.Threads) == 0 || len(journal.Threads) > maxThreadIDs ||
		len(journal.Remaining) > len(journal.Threads) {
		return errors.New("invalid recovery journal")
	}
	seen := make(map[string]bool, len(journal.Threads))
	for _, id := range journal.Threads {
		if id == "" || len(id) > 1024 || seen[id] {
			return errors.New("invalid recovery journal")
		}
		seen[id] = true
	}
	if journal.Threads[len(journal.Threads)-1] != journal.RootID {
		return errors.New("invalid recovery journal")
	}
	offset := len(journal.Threads) - len(journal.Remaining)
	for index, id := range journal.Remaining {
		if id != journal.Threads[offset+index] {
			return errors.New("invalid recovery journal")
		}
	}
	return nil
}
