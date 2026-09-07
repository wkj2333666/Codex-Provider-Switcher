package rollout

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestSanitizeEscapedAndSpacedIDKeys(t *testing.T) {
	for _, key := range []string{`"id" :`, `"\u0069d":`} {
		for _, compact := range []bool{false, true} {
			item := `{"type":"message",` + key + `"item_stale","content":[]}`
			line := `{"type":"response_item","payload":` + item + `}`
			if compact {
				line = `{"type":"compacted","payload":{"replacement_history":[` + item + `]}}`
			}
			clean, keep, changed, _, stripped, err := sanitizeLine([]byte(line), false)
			if err != nil || !keep || !changed || stripped != 1 || bytes.Contains(clean, []byte("item_stale")) {
				t.Fatalf("key=%s compact=%v keep=%v changed=%v stripped=%d err=%v", key, compact, keep, changed, stripped, err)
			}
		}
	}
}

func BenchmarkCleanRollout(b *testing.B) {
	line := []byte(`{"type":"event_msg","payload":{"text":"` + strings.Repeat("x", 64*1024) + `"}}` + "\n")
	data := bytes.Repeat(line, 512)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rewriteJSONL(bytes.NewReader(data), io.Discard, ""); err != nil {
			b.Fatal(err)
		}
	}
}

// Real compact records can contain very large tool outputs. A clean record must
// be inspected without copying each nested payload or encoding it again.
func BenchmarkCleanCompactedRollout(b *testing.B) {
	item := `{"type":"function_call_output","call_id":"call_ok","output":"` + strings.Repeat("x", 64*1024) + `"}`
	line := []byte(`{"type":"compacted","payload":{"replacement_history":[` + item + `]}}` + "\n")
	data := bytes.Repeat(line, 512)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rewriteJSONL(bytes.NewReader(data), io.Discard, ""); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSanitizeReusableBuffersAndMalformedTail(t *testing.T) {
	line := `{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_ok","output":"` + strings.Repeat("x", 200000) + `"}}` + "\n"
	original := []byte(line + `{"type":"compacted","payload":{"replacement_history":[{"type":"message","id":"msg_valid"}]}}` + "\n" + line)
	var out bytes.Buffer
	result, err := rewriteJSONL(bytes.NewReader(original), &out, "")
	if err != nil || result.Changed || !bytes.Equal(out.Bytes(), original) {
		t.Fatalf("result=%+v err=%v output unchanged=%v", result, err, bytes.Equal(out.Bytes(), original))
	}
	if _, err := rewriteJSONL(bytes.NewReader(append(original, []byte(`{"broken":`)...)), io.Discard, ""); err == nil {
		t.Fatal("malformed tail accepted")
	}
}

func TestSanitizeIgnoresCaseAliases(t *testing.T) {
	for _, item := range []string{
		`{"type":"message","id":"item_dirty","ID":"safe"}`,
		`{"type":"reasoning","Type":"message","id":"item_dirty"}`,
	} {
		for _, compact := range []bool{false, true} {
			line := `{"type":"response_item","payload":` + item + `}`
			if compact {
				line = `{"type":"compacted","payload":{"replacement_history":[` + item + `]}}`
			}
			_, _, changed, removed, stripped, err := sanitizeLine([]byte(line), false)
			if err != nil || !changed || removed+stripped != 1 {
				t.Fatalf("compacted=%v changed=%v removed=%d stripped=%d err=%v", compact, changed, removed, stripped, err)
			}
		}
	}
}

func TestSanitizeDuplicatePayloadUsesLastObject(t *testing.T) {
	line := []byte(`{"type":"response_item","payload":{"type":"message","id":"safe"},"payload":{"id":"safe"}}`)
	if _, _, _, _, _, err := sanitizeLine(line, false); err == nil {
		t.Fatal("merged duplicate payload hid missing type")
	}
}

func FuzzSanitizeFastPathMatchesConservative(f *testing.F) {
	for _, line := range []string{
		`{"type":"response_item","payload":{"type":"message","id":"item_stale","ID":"safe"}}`,
		`{"type":"compacted","payload":{"replacement_history":[{"type":"message","id":"item_stale"}]}}`,
		`{"type":"event_msg","payload":{"Type":"custom","text":"hello"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","id":"item_ui","ID":"unknown"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"safe"},"payload":{"id":"safe"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"safe"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"item_dirty","content":"quoted \"id\": text"}}`,
	} {
		f.Add([]byte(line))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		a, ak, ac, ar, as, ae := sanitizeLine(line, false)
		b, bk, bc, br, bs, be := sanitizeLineConservative(line, false)
		if (ae != nil) != (be != nil) {
			t.Fatalf("fast error=%v conservative error=%v input=%q", ae, be, line)
		}
		if ae == nil && (!bytes.Equal(a, b) || ak != bk || ac != bc || ar != br || as != bs) {
			t.Fatalf("fast and conservative differ for %q", line)
		}
	})
}
