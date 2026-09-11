package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAncestorsPreserveLegacyOffsetsAndRejectUnsafeLineage(t *testing.T) {
	for _, mode := range []string{"legacy", "writer", "cycle", "cutoff", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, "sessions", "2026", "09", "11")
			os.MkdirAll(dir, 0700)
			parent := filepath.Join(dir, "rollout-time-parent.jsonl")
			child := filepath.Join(dir, "rollout-time-child.jsonl")
			source := []byte(`{"type":"session_meta","payload":{"id":"parent"}}` + "\n" + `{"type":"response_item","payload":{"type":"reasoning","id":"item_bad"}}` + "\n")
			baseID := "parent"
			cutoff := len(source)
			if mode == "cycle" {
				baseID = "child"
			}
			if mode == "cutoff" {
				cutoff++
			}
			header := []byte(fmt.Sprintf(`{"ordinal":2,"type":"session_meta","payload":{"id":"child","history_base":{"thread_id":%q,"end_ordinal_exclusive":2,"end_byte_offset":%d}}}`+"\n", baseID, cutoff))
			os.WriteFile(parent, source, 0600)
			os.WriteFile(child, header, 0600)
			if mode == "symlink" {
				os.Rename(parent, parent+".real")
				os.Symlink(parent+".real", parent)
			}
			if mode == "writer" {
				lock := filepath.Join(home, "thread-writer-locks", "parent.lock")
				os.MkdirAll(filepath.Dir(lock), 0700)
				f, _ := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0600)
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			err := SanitizeAncestors(context.Background(), home, "child", child)
			if mode != "legacy" {
				if err == nil {
					t.Fatal("unsafe ancestor accepted")
				}
				if mode == "writer" && !errors.Is(err, ErrActiveWriter) {
					t.Fatal(err)
				}
				got, _ := os.ReadFile(parent)
				if !bytes.Equal(source, got) {
					t.Fatal("source changed on error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(parent)
			if len(got) != len(source) || bytes.Contains(got, []byte("item_bad")) {
				t.Fatal("legacy ancestor offsets not preserved")
			}
			for _, line := range bytes.Split(bytes.TrimSpace(got), []byte{'\n'}) {
				if !json.Valid(bytes.TrimSpace(line)) {
					t.Fatal("invalid JSON")
				}
			}
		})
	}
}
