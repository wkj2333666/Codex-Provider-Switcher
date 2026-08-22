package rollout

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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
