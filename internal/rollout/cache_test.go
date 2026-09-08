package rollout

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanScanCacheInvalidation(t *testing.T) {
	for _, mutation := range []string{"append", "truncate", "replace", "partial-tail", "same-size-restored-mtime", "malformed-tail"} {
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

func TestCleanAppendReusesCursorAndOnlyParsesSuffix(t *testing.T) {
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
	if err != nil || result.Cached || result.Changed || result.CachedPrefixBytes != int64(len(line)) {
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
		if result, err := SanitizeFile(path, lock, "thread-a"); err != nil || result.CachedPrefixBytes != int64(len(header)) {
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

// A sparse prefix makes any accidental re-read observable as invalid JSON.
// The cursor is trusted: previously scanned content is append-only.
func TestAppendScanSeeksPastCachedPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	const prefix = int64(64 << 20)
	if _, err := file.Seek(prefix, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"event_msg","payload":{"type":"token_count"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	previous := sanitationCertificate{Identity: sanitationIdentity(info, path, ""), Size: prefix, EndsInNewline: true}
	result, _, err := inspectSanitation(file, path, "", previous)
	if err != nil || result.Changed || result.CachedPrefixBytes != prefix {
		t.Fatalf("append read cached bytes: result=%+v err=%v", result, err)
	}
}

func TestScanCursorDoesNotStoreContentHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	lock := filepath.Join(dir, "locks", "thread.lock")
	if err := os.WriteFile(path, []byte(`{"type":"event_msg","payload":{}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SanitizeFile(path, lock, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(lock + ".sanitized")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"sha256"`)) {
		t.Fatal("cursor still stores a full-content hash")
	}
}

func TestLegacyHashCertificateReusesCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	lock := filepath.Join(dir, "locks", "thread.lock")
	line := []byte(`{"type":"event_msg","payload":{}}` + "\n")
	if err := os.WriteFile(path, line, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SanitizeFile(path, lock, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(lock + ".sanitized")
	if err != nil {
		t.Fatal(err)
	}
	// An old digest is intentionally ignored; migration needs no content reads.
	legacy := append([]byte(`{"sha256":"0000000000000000000000000000000000000000000000000000000000000000",`), data[1:]...)
	if err := os.WriteFile(lock+".sanitized", legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append([]byte{}, line...), line...), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := SanitizeFile(path, lock, "")
	if err != nil || got.CachedPrefixBytes != int64(len(line)) {
		t.Fatalf("legacy cursor not reused: %+v %v", got, err)
	}
	data, err = os.ReadFile(lock + ".sanitized")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"sha256"`)) {
		t.Fatal("legacy digest retained after cursor update")
	}
}

func TestAppendCursorUsesOpenedFileIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	clean := []byte(`{"type":"response_item","payload":{"type":"message","id":"safe_clean"}}` + "\n")
	dirty := []byte(`{"type":"response_item","payload":{"type":"message","id":"item_dirty"}}` + "\n")
	if len(clean) != len(dirty) {
		t.Fatal("fixture lengths differ")
	}
	if err := os.WriteFile(path, append(append([]byte{}, clean...), clean...), 0600); err != nil {
		t.Fatal(err)
	}
	stale, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := sanitationCertificate{Identity: sanitationIdentity(stale, path, ""), Size: int64(len(clean)), EndsInNewline: true}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, append(dirty, clean...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	// Simulate replacement after pathname stat and before open.
	got, _, err := inspectSanitation(opened, path, "", previous)
	if err != nil || !got.Changed || got.CachedPrefixBytes != 0 {
		t.Fatalf("replacement reused old cursor: %+v %v", got, err)
	}
}
