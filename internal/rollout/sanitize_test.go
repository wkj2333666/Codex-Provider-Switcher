package rollout

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSanitizePaginatedPreservesOffsetsAndUIHistory(t *testing.T) {
	lines := []string{
		`{"ordinal":0,"type":"session_meta","payload":{"id":"thr-a","history_mode":"paginated"}}`,
		`{"ordinal":1,"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","id":"item_stale","summary_text":["visible"]}}}`,
		`{"ordinal":2,"type":"response_item","payload":{"type":"reasoning","id":"item_stale","summary":[]}}`,
		`{"ordinal":3,"type":"response_item","payload":{"type":"message","id":"item_msg","role":"assistant","content":[{"type":"output_text","text":"keep <text> 中文"}]}}`,
		`{"ordinal":4,"type":"compacted","payload":{"replacement_history":[{"type":"reasoning","id":"item_checkpoint","summary":[]},{"type":"message","id":"item_msg","role":"assistant","content":[]}],"window_number":1}}`,
	}
	input := strings.Join(lines, "\n") + "\n"
	var out bytes.Buffer
	result, err := rewriteJSONL(strings.NewReader(input), &out, "thr-a")
	if err != nil {
		t.Fatal(err)
	}
	if result.ReasoningRemoved != 2 || result.IDsStripped != 2 {
		t.Fatalf("result = %#v", result)
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != len(lines) {
		t.Fatalf("record count changed: %d", len(got))
	}
	for i := range lines {
		if len(got[i]) != len(lines[i]) {
			t.Fatalf("record %d shifted byte offsets", i)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(got[i]), &v); err != nil {
			t.Fatal(err)
		}
		if v["ordinal"] != float64(i) {
			t.Fatalf("ordinal changed: %v", v)
		}
	}
	if got[1] != lines[1] {
		t.Fatal("UI event changed")
	}
	if strings.Contains(got[2], "item_stale") || !strings.Contains(got[2], `"type":"other"`) {
		t.Fatal("reasoning not neutralized")
	}
	if strings.Contains(got[4], "item_checkpoint") || strings.Contains(got[4], "item_msg") {
		t.Fatal("compact history was not cleaned")
	}
	var second bytes.Buffer
	again, err := rewriteJSONL(strings.NewReader(out.String()), &second, "thr-a")
	if err != nil || again.Changed || second.String() != out.String() {
		t.Fatalf("not idempotent: %#v %v", again, err)
	}
}

func TestSanitizePaginatedRejectsUnnumberedRecordWithoutWriting(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rollout.jsonl")
	original := `{"ordinal":0,"type":"session_meta","payload":{"id":"thr-a","history_mode":"paginated"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"reasoning","id":"item_bad","summary":[]}}` + "\n"
	if err := os.WriteFile(file, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SanitizeFile(file, "", "thr-a"); err == nil {
		t.Fatal("accepted missing paginated ordinal")
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != original {
		t.Fatal("malformed paginated file changed")
	}
}

func TestSanitizeFileRemovesProviderBoundReasoningAndStripsStaleItemIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	input := strings.Join([]string{
		`{"timestamp":"1","type":"event_msg","payload":{"type":"compacted"}}`,
		`{"timestamp":"2","type":"response_item","payload":{"type":"reasoning","id":"item_stale","summary":[],"encrypted_content":"provider-bound"}}`,
		`{"timestamp":"3","type":"response_item","payload":{"type":"message","id":"item_message","role":"assistant","content":[{"type":"output_text","text":"keep"}]}}`,
		`{"timestamp":"4","type":"response_item","payload":{"type":"function_call","id":"item_call","call_id":"call_1","name":"tool","arguments":"{}"}}`,
		`{"timestamp":"5","type":"response_item","payload":{"type":"function_call_output","id":"fco_1","call_id":"call_1","output":"ok"}}`,
		`{"timestamp":"6","type":"response_item","payload":{"type":"reasoning","id":"rs_valid","summary":[],"encrypted_content":"keep"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := SanitizeFile(path, "", "")
	if err != nil {
		t.Fatalf("SanitizeFile() error = %v", err)
	}
	if !result.Changed || result.ReasoningRemoved != 1 || result.IDsStripped != 2 {
		t.Fatalf("SanitizeFile() result = %#v", result)
	}
	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if strings.Contains(text, `"id":"item_stale"`) || strings.Contains(text, "provider-bound") {
		t.Fatalf("stale reasoning survived: %s", text)
	}
	if strings.Contains(text, `"id":"item_message"`) || strings.Contains(text, `"id":"item_call"`) {
		t.Fatalf("stale item ID survived: %s", text)
	}
	if !strings.Contains(text, `"id":"fco_1"`) || !strings.Contains(text, `"id":"rs_valid"`) || !strings.Contains(text, `"text":"keep"`) {
		t.Fatalf("valid rollout data was not preserved: %s", text)
	}
}

func TestSanitizeFileDoesNotRewriteWhenInputIsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	original := []byte(`{"type":"response_item","payload":` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := SanitizeFile(path, "", "")
	if err == nil {
		t.Fatal("SanitizeFile() error = nil, want malformed JSON error")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("malformed input changed from %q to %q", original, got)
	}
}

func TestSanitizeFileRejectsUnknownResponseItemShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	original := []byte(`{"type":"response_item","payload":{"type":"reasoning","id":42}}` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := SanitizeFile(path, "", "")
	if err == nil {
		t.Fatal("SanitizeFile() error = nil, want unknown shape error")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("unknown response item changed from %q to %q", original, got)
	}
}

func TestSanitizeFileRefusesActiveWriterLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	lockPath := filepath.Join(dir, "thread.lock")
	if err := os.WriteFile(path, []byte(`{"type":"response_item","payload":{"type":"reasoning","id":"item_stale"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	_, err = SanitizeFile(path, lockPath, "")
	if !errors.Is(err, ErrActiveWriter) {
		t.Fatalf("SanitizeFile() error = %v, want ErrActiveWriter", err)
	}
}

func TestSanitizeFileAllowsCleanRolloutWithActiveWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	lockPath := filepath.Join(dir, "thread.lock")
	clean := []byte(`{"type":"response_item","payload":{"type":"message","id":"msg_valid","role":"assistant","content":[]}}` + "\n")
	if err := os.WriteFile(path, clean, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	result, err := SanitizeFile(path, lockPath, "")
	if err != nil || result.Changed {
		t.Fatalf("SanitizeFile() = %#v, %v; want clean no-op", result, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(clean) {
		t.Fatalf("clean rollout changed from %q to %q", clean, got)
	}
}

func TestSanitizeFileCreatesMissingWriterLockBeforeRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	lockPath := filepath.Join(dir, "thread.lock")
	if err := os.WriteFile(path, []byte(`{"type":"response_item","payload":{"type":"reasoning","id":"item_stale"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := SanitizeFile(path, lockPath, "")
	if err != nil || !result.Changed {
		t.Fatalf("SanitizeFile() = %#v, %v", result, err)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writer lock was not cleaned up, lstat error = %v", err)
	}
}

func TestValidatePathRejectsUnsafeAndSymlinkedRollouts(t *testing.T) {
	home := t.TempDir()
	dateDir := filepath.Join(home, "sessions", "2026", "08", "20")
	if err := os.MkdirAll(dateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	threadID := "01abc-def"
	want := filepath.Join(dateDir, "rollout.jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(home, threadID, want); err != nil {
		t.Fatalf("ValidatePath() error = %v", err)
	}
	if err := ValidatePath(home, "../escape", want); err == nil {
		t.Fatal("ValidatePath() accepted unsafe thread ID")
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(home, threadID, outside); err == nil {
		t.Fatal("ValidatePath() accepted path outside sessions")
	}
	linked := filepath.Join(dateDir, "linked.jsonl")
	if err := os.Symlink(want, linked); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(home, threadID, linked); err == nil {
		t.Fatal("ValidatePath() accepted symlinked rollout")
	}
}
