package rollout

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// ContextPolicy applies only to the task's own rollout, never shared ancestors.
// Native pre-turn compaction reuses turn_context.model with the current provider.
// Models allowlisted for that provider retain normal model-switch compaction.
type ContextPolicy struct {
	Model           string
	Compatible      []string
	PreserveOffsets bool
	audit           io.Writer
}

func contextPolicy(policies []ContextPolicy) ContextPolicy {
	if len(policies) > 0 {
		return policies[0]
	}
	return ContextPolicy{}
}

func (p ContextPolicy) key() string {
	if p.Model == "" && !p.PreserveOffsets {
		return ""
	}
	models := append([]string(nil), p.Compatible...)
	sort.Strings(models)
	data, _ := json.Marshal(struct {
		Version         int
		Model           string
		Compatible      []string
		PreserveOffsets bool
	}{1, p.Model, models, p.PreserveOffsets})
	return fmt.Sprintf("%x", sha256.Sum256(data)) // Small policy metadata only.
}

func (p ContextPolicy) clean(line []byte) ([]byte, bool, error) {
	if p.Model == "" || !bytes.Contains(line, []byte("turn_context")) {
		return line, false, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, false, err
	}
	var kind string
	if json.Unmarshal(envelope["type"], &kind) != nil || kind != "turn_context" {
		return line, false, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(envelope["payload"], &payload); err != nil {
		return nil, false, err
	}
	var model string
	if json.Unmarshal(payload["model"], &model) != nil || model == "" || model == p.Model {
		return line, false, nil
	}
	for _, compatible := range p.Compatible {
		if model == compatible {
			return line, false, nil
		}
	}
	// Patch only scalar fields in place. Re-encoding the whole envelope can
	// reorder keys and expand a paginated record, invalidating byte cutoffs.
	encoded := append([]byte(nil), line...)
	var ok bool
	encoded, ok = replaceJSONStringField(encoded, "model", p.Model)
	if !ok {
		return nil, false, fmt.Errorf("context model field missing")
	}
	encoded, _ = replaceRawField(encoded, "comp_hash", []byte("null"))
	if p.audit != nil {
		if _, err := p.audit.Write(line); err != nil {
			return nil, false, err
		}
	}
	return encoded, true, nil
}

func replaceJSONStringField(line []byte, key, value string) ([]byte, bool) {
	needle := []byte(`"` + key + `"`)
	at := bytes.Index(line, needle)
	if at < 0 {
		return line, false
	}
	colon := bytes.IndexByte(line[at+len(needle):], ':')
	if colon < 0 {
		return line, false
	}
	start := at + len(needle) + colon + 1
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	if start >= len(line) || line[start] != '"' {
		return line, false
	}
	end := start + 1
	for end < len(line) {
		if line[end] == '"' && line[end-1] != '\\' {
			break
		}
		end++
	}
	if end >= len(line) {
		return line, false
	}
	encoded, _ := json.Marshal(value)
	out := make([]byte, 0, len(line)+len(encoded)-(end-start+1))
	out = append(out, line[:start]...)
	out = append(out, encoded...)
	out = append(out, line[end+1:]...)
	return out, true
}

func replaceRawField(line []byte, key string, value []byte) ([]byte, bool) {
	needle := []byte(`"` + key + `"`)
	at := bytes.Index(line, needle)
	if at < 0 {
		return line, false
	}
	colon := bytes.IndexByte(line[at+len(needle):], ':')
	if colon < 0 {
		return line, false
	}
	start := at + len(needle) + colon + 1
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	end := start
	if end < len(line) && line[end] == '"' {
		end++
		for end < len(line) {
			if line[end] == '"' && line[end-1] != '\\' {
				end++
				break
			}
			end++
		}
	} else {
		for end < len(line) && line[end] != ',' && line[end] != '}' && line[end] != '\n' {
			end++
		}
	}
	out := make([]byte, 0, len(line)+len(value))
	out = append(out, line[:start]...)
	out = append(out, value...)
	out = append(out, line[end:]...)
	return out, true
}
