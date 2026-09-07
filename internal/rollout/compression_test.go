package rollout

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func compressedFixture(t *testing.T) (string, string, string, []byte) {
	t.Helper()
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd required for compressed history fixtures")
	}
	home := t.TempDir()
	dir := filepath.Join(home, "sessions")
	os.Mkdir(dir, 0700)
	path := filepath.Join(dir, "rollout.jsonl")
	data := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"thr-a\"}}\n")
	os.WriteFile(path, data, 0600)
	if out, err := exec.Command("zstd", "-q", "--rm", path).CombinedOutput(); err != nil {
		t.Fatalf("compress: %v %s", err, out)
	}
	return home, path, filepath.Join(home, "thread-writer-locks", "thr-a.lock"), data
}

func TestCompressedHistoryMaterializesCanonicalAndPhysicalPaths(t *testing.T) {
	for _, suffix := range []string{"", ".zst"} {
		t.Run(suffix, func(t *testing.T) {
			home, path, lock, data := compressedFixture(t)
			got, err := PreparePlain(context.Background(), home, "thr-a", path+suffix, lock)
			if err != nil || got != path {
				t.Fatalf("restore: %q %v", got, err)
			}
			actual, _ := os.ReadFile(path)
			if string(actual) != string(data) {
				t.Fatal("history changed")
			}
			if _, err := os.Stat(path + ".zst"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("compressed duplicate remains")
			}
			if _, err := SanitizeFile(path, lock, "thr-a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompressedHistoryFailurePreservesSource(t *testing.T) {
	for _, kind := range []string{"corrupt", "busy", "canceled", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			home, path, lock, _ := compressedFixture(t)
			ctx := context.Background()
			switch kind {
			case "corrupt":
				os.WriteFile(path+".zst", []byte("not zstd"), 0600)
			case "busy":
				unlock, err := acquireWriterLock(lock)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "symlink":
				original := path + ".zst"
				os.Rename(original, original+".real")
				os.Symlink(original+".real", original)
			}
			before, _ := os.ReadFile(path + ".zst")
			if _, err := PreparePlain(ctx, home, "thr-a", path, lock); err == nil {
				t.Fatal("unsafe restoration succeeded")
			}
			after, _ := os.ReadFile(path + ".zst")
			if string(before) != string(after) {
				t.Fatal("source changed")
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial output published")
			}
		})
	}
}

func TestHistoryAccessRecordsReadAndRejectsUnsafeID(t *testing.T) {
	home := t.TempDir()
	NoteAccess(home, "thr-a")
	path := filepath.Join(home, "codex-provider-switcher", "history-access", "thr-a")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("access marker: %v %v", info, err)
	}
	NoteAccess(home, "../escape")
	if _, err := os.Stat(filepath.Join(home, "codex-provider-switcher", "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsafe marker created")
	}
}
