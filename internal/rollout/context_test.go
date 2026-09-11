package rollout

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderContextRepairPreservesOffsetsAndInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	lock := filepath.Join(dir, "locks", "thr.lock")
	lines := []string{
		`{"ordinal":0,"type":"session_meta","payload":{"id":"thr","history_mode":"paginated"}}`,
		`{"ordinal":1,"type":"turn_context","payload":{"model":"glm-5.3-flash","comp_hash":"3000","effort":"max","cwd":"/tmp","turn_id":"previous"}}`,
		`{"ordinal":2,"type":"event_msg","payload":{"type":"thread_settings_applied","model":"glm-5.3-flash"}}`,
		`{"ordinal":3,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep me"}]}}`,
		`{"ordinal":4,"type":"turn_context","payload":{"model":"gpt-5.6-sol","comp_hash":"same-provider"}}`,
	}
	original := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SanitizeFile(path, lock, "thr"); err != nil {
		t.Fatal(err)
	} // legacy generic certificate
	policy := ContextPolicy{Model: "gpt-6-astra", Compatible: []string{"gpt-6-astra", "gpt-5.6-sol"}}
	r, err := SanitizeFile(path, lock, "thr", policy)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	after := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if !r.Changed || !strings.Contains(after[1], `"model":"gpt-6-astra"`) || strings.Contains(after[1], "comp_hash") {
		t.Fatal("previous provider model survived repair")
	}
	for i := range lines {
		if len(after[i]) != len(lines[i]) {
			t.Fatal("offset changed")
		}
		if i != 1 && after[i] != lines[i] {
			t.Fatal("unrelated history changed")
		}
	}
	audits, _ := filepath.Glob(filepath.Join(dir, "locks", "thr.lock.context-*.jsonl"))
	if len(audits) != 1 {
		t.Fatalf("original context audit missing: %v", audits)
	}
	audit, _ := os.ReadFile(audits[0])
	if !bytes.Equal(audit, []byte(lines[1]+"\n")) {
		t.Fatal("original context not preserved")
	}
	if _, err := SanitizeFile(path, lock, "thr", policy); err != nil {
		t.Fatal(err)
	}
	r, err = SanitizeFile(path, lock, "thr", policy)
	if err != nil || !r.Cached {
		t.Fatalf("cache not reused: %+v %v", r, err)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(`{"ordinal":5,"type":"event_msg","payload":{"type":"test"}}` + "\n")
	f.Close()
	r, err = SanitizeFile(path, lock, "thr", policy)
	if err != nil || r.CachedPrefixBytes != int64(len(got)) {
		t.Fatalf("append cursor not reused: %+v %v", r, err)
	}
	policy = ContextPolicy{Model: "glm-5.3-flash", Compatible: []string{"glm-5.3-flash"}}
	r, err = SanitizeFile(path, lock, "thr", policy)
	if err != nil || !r.Changed {
		t.Fatalf("route change reused stale certificate: %+v %v", r, err)
	}
}

func TestProviderContextRepairRefusesOffsetExpansion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	original := []byte(`{"ordinal":0,"type":"turn_context","payload":{"model":"x"}}` + "\n")
	os.WriteFile(path, original, 0600)
	_, err := SanitizeFile(path, filepath.Join(dir, "thr.lock"), "", ContextPolicy{Model: strings.Repeat("long", 100)})
	got, _ := os.ReadFile(path)
	if err == nil || !bytes.Equal(got, original) {
		t.Fatal("expanding repair must leave source untouched")
	}
}
