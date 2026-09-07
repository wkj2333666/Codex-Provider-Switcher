package rollout

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanScanCacheInvalidation(t *testing.T) {
	for _, mutation := range []string{"append", "rewrite-and-grow", "truncate", "replace", "partial-tail", "same-size-restored-mtime", "malformed-tail"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			path, lock := filepath.Join(dir, "rollout.jsonl"), filepath.Join(dir, "locks", "thread.lock")
			clean := []byte(`{"type":"response_item","payload":{"type":"message","id":"safe_clean"}}` + "\n")
			dirty := []byte(`{"type":"response_item","payload":{"type":"message","id":"item_dirty"}}` + "\n")
			if mutation == "partial-tail" {
				clean = bytes.TrimSuffix(clean, []byte("\n"))
			}
			if err := os.WriteFile(path, clean, 0600); err != nil {
				t.Fatal(err)
			}
			first, err := SanitizeFile(path, lock, "")
			if err != nil || first.Cached {
				t.Fatalf("first=%+v err=%v", first, err)
			}
			cached, err := SanitizeFile(path, lock, "")
			if err != nil || !cached.Cached {
				t.Fatalf("unchanged=%+v err=%v", cached, err)
			}
			before, _ := os.Stat(path)
			next := dirty
			switch mutation {
			case "append":
				next = append(append([]byte{}, clean...), dirty...)
			case "rewrite-and-grow":
				next = append(append([]byte{}, dirty...), clean...)
			case "truncate":
				next = clean[:len(clean)/2]
			case "replace":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "partial-tail":
				next = append(append(append([]byte{}, clean...), '\n'), dirty...)
			case "malformed-tail":
				next = append(append([]byte{}, clean...), []byte("{bad")...)
			}
			if err := os.WriteFile(path, next, 0600); err != nil {
				t.Fatal(err)
			}
			if mutation == "same-size-restored-mtime" {
				if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			result, err := SanitizeFile(path, lock, "")
			if result.Cached {
				t.Fatal("changed history reused clean scan")
			}
			if mutation == "truncate" || mutation == "malformed-tail" {
				if err == nil {
					t.Fatal("malformed history accepted")
				}
			} else if err != nil || !result.Changed {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestCleanScanCacheIsBoundToThreadAndFailsOpenToScanning(t *testing.T) {
	dir := t.TempDir()
	path, lock := filepath.Join(dir, "rollout.jsonl"), filepath.Join(dir, "locks", "thread.lock")
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"thread-a"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SanitizeFile(path, lock, "thread-a"); err != nil {
		t.Fatal(err)
	}
	if result, err := SanitizeFile(path, lock, "thread-b"); err == nil || result.Cached {
		t.Fatal("certificate bypassed session identity validation")
	}
	if err := os.WriteFile(lock+".sanitized", []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := SanitizeFile(path, lock, "thread-a"); err != nil || result.Cached {
		t.Fatalf("corrupt certificate result=%+v err=%v", result, err)
	}
	if result, err := SanitizeFile(path, lock, "thread-a"); err != nil || !result.Cached {
		t.Fatalf("certificate was not rebuilt: result=%+v err=%v", result, err)
	}
}

func TestCleanAppendVerifiesPrefixAndOnlyParsesSuffix(t *testing.T) {
	dir := t.TempDir()
	path, lock := filepath.Join(dir, "rollout.jsonl"), filepath.Join(dir, "locks", "thread.lock")
	line := []byte(`{"type":"event_msg","payload":{"text":"clean"}}` + "\n")
	if err := os.WriteFile(path, line, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SanitizeFile(path, lock, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append([]byte{}, line...), line...), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := SanitizeFile(path, lock, "")
	if err != nil || result.Cached || result.Changed || result.VerifiedPrefixBytes != int64(len(line)) {
		t.Fatalf("append=%+v err=%v", result, err)
	}
	result, err = SanitizeFile(path, lock, "")
	if err != nil || !result.Cached {
		t.Fatalf("unchanged append=%+v err=%v", result, err)
	}
}

func TestAppendPreservesOriginalPaginationContext(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		dir := t.TempDir()
		path, lock := filepath.Join(dir, "rollout.jsonl"), filepath.Join(dir, "locks", "thread.lock")
		header := `{"type":"session_meta","payload":{"id":"thread-a"}}` + "\n"
		if paginated {
			header = `{"ordinal":0,"type":"session_meta","payload":{"id":"thread-a","history_mode":"paginated"}}` + "\n"
		}
		if err := os.WriteFile(path, []byte(header), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := SanitizeFile(path, lock, "thread-a"); err != nil {
			t.Fatal(err)
		}
		// A suffix record is not a new session_meta. The full scanner does not
		// apply the session header's schema to a clean event's extension fields.
		clean := `{"ordinal":"extension","type":"event_msg","payload":{"history_mode":123}}` + "\n"
		if err := os.WriteFile(path, []byte(header+clean), 0600); err != nil {
			t.Fatal(err)
		}
		if result, err := SanitizeFile(path, lock, "thread-a"); err != nil || result.VerifiedPrefixBytes != int64(len(header)) {
			t.Fatalf("pagination=%v result=%+v err=%v", paginated, result, err)
		}
		dirty := `{"type":"response_item","payload":{"type":"reasoning","id":"item_dirty"}}` + "\n"
		if err := os.WriteFile(path, []byte(header+clean+dirty), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := SanitizeFile(path, lock, "thread-a")
		if paginated && err == nil {
			t.Fatal("incremental scan forgot paginated ordinal requirement")
		}
		if !paginated && err != nil {
			t.Fatal(err)
		}
	}
}
