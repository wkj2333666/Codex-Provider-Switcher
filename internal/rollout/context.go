package rollout

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	if p.Model == "" || !bytes.Contains(line, []byte("turn_context")) && !bytes.Contains(line, []byte(`\u`)) {
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
	payload["model"], _ = json.Marshal(p.Model)
	delete(payload, "comp_hash")
	var err error
	envelope["payload"], err = marshalUnescaped(payload)
	if err != nil {
		return nil, false, err
	}
	encoded, err := marshalUnescaped(envelope)
	if err != nil {
		return nil, false, err
	}
	trimmed := bytes.TrimSuffix(line, []byte{'\n'})
	// Preserve offsets for legacy files too: a paginated fork can refer to them.
	if len(encoded) > len(trimmed) {
		return nil, false, errors.New("context repair would shift rollout offsets")
	}
	encoded = append(encoded, bytes.Repeat([]byte{' '}, len(trimmed)-len(encoded))...)
	if bytes.HasSuffix(line, []byte{'\n'}) {
		encoded = append(encoded, '\n')
	}
	if p.audit != nil {
		if _, err := p.audit.Write(line); err != nil {
			return nil, false, err
		}
	}
	return encoded, true, nil
}
