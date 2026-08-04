package recovery

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStoreRoundTripsPrivateJournal(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	want := Journal{
		Version: 1, RootID: "root-a", Provider: "sub2api", Phase: "restoring",
		Threads: []string{"child-a", "root-a"}, Remaining: []string{"child-a", "root-a"},
	}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Load("root-a")
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, %v, %v; want %#v, true, nil", got, ok, err, want)
	}
	info, err := os.Stat(store.path("root-a"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %o, want 600", info.Mode().Perm())
	}
	directory, err := os.Stat(filepath.Dir(store.path("root-a")))
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0o700 {
		t.Fatalf("journal directory mode = %o, want 700", directory.Mode().Perm())
	}
	if err := store.Clear("root-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Load("root-a"); err != nil || ok {
		t.Fatalf("Load() after Clear = ok %v, err %v", ok, err)
	}
}

func TestStoreRoundTripsMappedRouteJournalVersionTwo(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := Journal{
		Version: 2, RootID: "root-glm", Provider: "glm", Model: "glm-5.2", Phase: "prepared",
		Threads: []string{"root-glm"}, Remaining: []string{"root-glm"},
	}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Load("root-glm")
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, %v, %v; want %#v, true, nil", got, ok, err, want)
	}
}

func TestStoreFailsClosedForInvalidOrCorruptJournal(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	invalid := []Journal{
		{},
		{Version: 2, RootID: "root", Provider: "openai", Phase: "prepared", Threads: []string{"root"}, Remaining: []string{"root"}},
		{Version: 1, RootID: "root", Provider: "glm", Model: "glm-5.2", Phase: "prepared", Threads: []string{"root"}, Remaining: []string{"root"}},
		{Version: 2, RootID: "root", Provider: "glm", Model: "bad model", Phase: "prepared", Threads: []string{"root"}, Remaining: []string{"root"}},
		{Version: 1, RootID: "other", Provider: "openai", Phase: "prepared", Threads: []string{"root"}, Remaining: []string{"root"}},
		{Version: 1, RootID: "root", Provider: "bad provider", Phase: "prepared", Threads: []string{"root"}, Remaining: []string{"root"}},
		{Version: 1, RootID: "root", Provider: "openai", Phase: "unknown", Threads: []string{"root"}, Remaining: []string{"root"}},
		{Version: 1, RootID: "root", Provider: "openai", Phase: "prepared", Threads: []string{"root"}, Remaining: []string{"root", "root"}},
		{Version: 1, RootID: "root", Provider: "openai", Phase: "prepared", Threads: []string{"child", "root"}, Remaining: []string{"root", "child"}},
	}
	for _, journal := range invalid {
		if err := store.Save(journal); err == nil {
			t.Fatalf("Save(%#v) error = nil", journal)
		}
	}

	if err := os.WriteFile(store.path("root"), []byte(`{"version":1,"rootId":"root"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("root"); err == nil {
		t.Fatal("Load() error = nil for corrupt journal")
	}
	if err := os.WriteFile(store.path("root"), make([]byte, maxJournalSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("root"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() oversized error = %v", err)
	}
}

func TestStoreRejectsSymlinkJournal(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	journal := Journal{
		Version: 1, RootID: "root", Provider: "openai", Phase: "prepared",
		Threads: []string{"root"}, Remaining: []string{"root"},
	}
	encoded, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.path("root")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("root"); err == nil {
		t.Fatal("Load() error = nil for symlink journal")
	}
}
