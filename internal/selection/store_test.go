package selection

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRoundTripUsesPrivateHashedState(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "state")
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok, err := store.Get("thread-secret-a"); err != nil || ok || got != "" {
		t.Fatalf("Get(missing) = %q, %v, %v", got, ok, err)
	}
	if err := store.Set("thread-secret-a", "sub2api"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.Get("thread-secret-a"); err != nil || !ok || got != "sub2api" {
		t.Fatalf("Get() = %q, %v, %v", got, ok, err)
	}

	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if permission := directoryInfo.Mode().Perm(); permission != 0o700 {
		t.Fatalf("state directory mode = %o", permission)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || strings.Contains(entries[0].Name(), "thread-secret-a") {
		t.Fatalf("state entries = %#v", entries)
	}
	fileInfo, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if permission := fileInfo.Mode().Perm(); permission != 0o600 {
		t.Fatalf("state file mode = %o", permission)
	}
}

func TestStoreRejectsInvalidAndCorruptStateWithoutLeaking(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "state")
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("thread-secret-b", "secret invalid provider"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Set(invalid) error = %v", err)
	}
	if err := store.Set("thread-secret-b", "openai"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir() = %#v, %v", entries, err)
	}
	path := filepath.Join(directory, entries[0].Name())
	if err := os.WriteFile(path, []byte("secret invalid provider\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get("thread-secret-b"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Get(corrupt) error = %v", err)
	}
}

func TestOpenRejectsSymlinkStateDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("Open(symlink) error = nil")
	}
}
