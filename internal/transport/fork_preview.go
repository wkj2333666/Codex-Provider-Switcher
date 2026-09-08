package transport

import (
	"context"
	"encoding/json"
	"strings"
)

// completeForkPreview runs after native fork creation but before Desktop sees
// the success response. The database and response must agree before Desktop
// refreshes thread/list, which otherwise drops empty-preview paginated forks.
func (current *session) completeForkPreview(ctx context.Context, payload []byte) ([]byte, []byte) {
	if current.codexHome == "" || current.repairForkPreview == nil {
		return payload, nil
	}
	var envelope map[string]json.RawMessage
	var result map[string]json.RawMessage
	var thread map[string]json.RawMessage
	if json.Unmarshal(payload, &envelope) != nil || json.Unmarshal(envelope["result"], &result) != nil ||
		json.Unmarshal(result["thread"], &thread) != nil || thread == nil {
		return payload, nil
	}
	var fields struct {
		ID          string `json:"id"`
		HistoryMode string `json:"historyMode"`
		Preview     string `json:"preview"`
		Ephemeral   bool   `json:"ephemeral"`
	}
	if json.Unmarshal(result["thread"], &fields) != nil || fields.ID == "" ||
		fields.HistoryMode != "paginated" || fields.Ephemeral || strings.TrimSpace(fields.Preview) != "" {
		return payload, nil
	}
	preview, err := current.repairForkPreview(ctx, current.codexHome, fields.ID)
	if err != nil {
		warning, _ := json.Marshal(map[string]any{
			"method": "warning",
			"params": map[string]string{
				"threadId": fields.ID,
				"message":  "The fork was created, but its task-list entry could not be repaired. Check Python 3.11+ and SQLite access. The fork already exists; avoid creating another copy.",
			},
		})
		return payload, warning
	}
	if strings.TrimSpace(preview) == "" {
		return payload, nil
	}
	thread["preview"] = rawJSONString(preview)
	result["thread"], _ = json.Marshal(thread)
	envelope["result"], _ = json.Marshal(result)
	updated, err := json.Marshal(envelope)
	if err != nil {
		return payload, nil
	}
	return updated, nil
}
