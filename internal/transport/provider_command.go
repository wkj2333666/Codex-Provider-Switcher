package transport

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	providerid "github.com/wkj2333666/Codex-Provider-Switcher/internal/provider"
)

const invalidProviderCommandMessage = "invalid provider command; use /provider status or /provider switch <name>"

type providerCommandAction uint8

const (
	providerCommandNone providerCommandAction = iota
	providerCommandStatus
	providerCommandSwitch
)

type providerCommand struct {
	action   providerCommandAction
	provider string
}

type commandInputItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func parseProviderCommand(message rpcMessage) (providerCommand, bool, error) {
	if message.method != "turn/start" || message.params == nil {
		return providerCommand{}, false, nil
	}
	var rawItems []json.RawMessage
	if json.Unmarshal(message.params["input"], &rawItems) != nil || len(rawItems) == 0 {
		return providerCommand{}, false, nil
	}

	items := make([]commandInputItem, 0, len(rawItems))
	providerSkills := 0
	malformedItems := false
	for _, raw := range rawItems {
		var item commandInputItem
		if json.Unmarshal(raw, &item) != nil || item.Type == "" {
			malformedItems = true
			continue
		}
		items = append(items, item)
		if item.Type == "skill" && item.Name == "provider" {
			providerSkills++
		}
	}

	commandIndex := -1
	commandMarker := ""
	var commandArguments []string
	for index, item := range items {
		if item.Type != "text" {
			continue
		}
		fields := strings.Fields(item.Text)
		if len(fields) == 0 {
			continue
		}
		marker := ""
		arguments := []string(nil)
		switch {
		case fields[0] == "/provider" || fields[0] == "$provider":
			marker = fields[0]
			arguments = fields[1:]
		default:
			continue
		}
		if commandIndex != -1 {
			return providerCommand{}, true, errors.New(invalidProviderCommandMessage)
		}
		commandIndex = index
		commandMarker = marker
		commandArguments = arguments
	}

	if commandIndex == -1 {
		if providerSkills != 0 {
			return providerCommand{}, true, errors.New(invalidProviderCommandMessage)
		}
		return providerCommand{}, false, nil
	}
	if malformedItems {
		return providerCommand{}, true, errors.New(invalidProviderCommandMessage)
	}
	if commandMarker == "$provider" {
		if providerSkills == 0 {
			return providerCommand{}, false, nil
		}
		if providerSkills != 1 || len(items) != 2 {
			return providerCommand{}, true, errors.New(invalidProviderCommandMessage)
		}
		skillValid := false
		for _, item := range items {
			if item.Type == "skill" && item.Name == "provider" && item.Path != "" {
				skillValid = true
			}
		}
		if !skillValid {
			return providerCommand{}, true, errors.New(invalidProviderCommandMessage)
		}
	} else if len(items) != 1 {
		return providerCommand{}, true, errors.New(invalidProviderCommandMessage)
	}

	switch {
	case len(commandArguments) == 1 && commandArguments[0] == "status":
		return providerCommand{action: providerCommandStatus}, true, nil
	case len(commandArguments) == 2 && commandArguments[0] == "switch" && providerid.Valid(commandArguments[1]):
		return providerCommand{action: providerCommandSwitch, provider: commandArguments[1]}, true, nil
	default:
		return providerCommand{}, true, errors.New(invalidProviderCommandMessage)
	}
}

type syntheticTurn struct {
	ID          string `json:"id"`
	Items       []any  `json:"items"`
	ItemsView   string `json:"itemsView"`
	Status      string `json:"status"`
	Error       any    `json:"error"`
	StartedAt   *int64 `json:"startedAt"`
	CompletedAt *int64 `json:"completedAt"`
	DurationMS  *int64 `json:"durationMs"`
}

type syntheticUserInput struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	TextElements []any  `json:"text_elements"`
}

type syntheticUserItem struct {
	Type     string               `json:"type"`
	ID       string               `json:"id"`
	ClientID any                  `json:"clientId"`
	Content  []syntheticUserInput `json:"content"`
}

type syntheticAgentItem struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	Text           string `json:"text"`
	Phase          string `json:"phase"`
	MemoryCitation any    `json:"memoryCitation"`
}

type syntheticResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type syntheticNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

func encodeProviderControlTurn(id json.RawMessage, threadID, commandText, feedback string, now time.Time) ([][]byte, error) {
	if len(id) == 0 || threadID == "" || commandText == "" || feedback == "" {
		return nil, errors.New("invalid synthetic provider turn")
	}
	turnID, err := syntheticID("turn")
	if err != nil {
		return nil, err
	}
	userItemID, err := syntheticID("user")
	if err != nil {
		return nil, err
	}
	agentItemID, err := syntheticID("agent")
	if err != nil {
		return nil, err
	}

	startedAt := now.Unix()
	startedAtMS := now.UnixMilli()
	userItem := syntheticUserItem{
		Type:     "userMessage",
		ID:       userItemID,
		ClientID: nil,
		Content: []syntheticUserInput{{
			Type:         "text",
			Text:         commandText,
			TextElements: []any{},
		}},
	}
	startedAgentItem := syntheticAgentItem{
		Type: "agentMessage", ID: agentItemID, Text: "", Phase: "final_answer", MemoryCitation: nil,
	}
	completedAgentItem := startedAgentItem
	completedAgentItem.Text = feedback
	startedTurn := syntheticTurn{
		ID: turnID, Items: []any{}, ItemsView: "notLoaded", Status: "inProgress", Error: nil,
	}
	notifiedTurn := startedTurn
	notifiedTurn.StartedAt = &startedAt
	completedTurn := syntheticTurn{
		ID: turnID, Items: []any{completedAgentItem}, ItemsView: "summary", Status: "completed", Error: nil,
		StartedAt: &startedAt, CompletedAt: &startedAt,
	}

	objects := []any{
		syntheticResponse{JSONRPC: "2.0", ID: id, Result: struct {
			Turn syntheticTurn `json:"turn"`
		}{Turn: startedTurn}},
		syntheticNotification{JSONRPC: "2.0", Method: "turn/started", Params: struct {
			ThreadID string        `json:"threadId"`
			Turn     syntheticTurn `json:"turn"`
		}{ThreadID: threadID, Turn: notifiedTurn}},
		syntheticNotification{JSONRPC: "2.0", Method: "item/started", Params: struct {
			Item        syntheticUserItem `json:"item"`
			ThreadID    string            `json:"threadId"`
			TurnID      string            `json:"turnId"`
			StartedAtMS int64             `json:"startedAtMs"`
		}{Item: userItem, ThreadID: threadID, TurnID: turnID, StartedAtMS: startedAtMS}},
		syntheticNotification{JSONRPC: "2.0", Method: "item/completed", Params: struct {
			Item          syntheticUserItem `json:"item"`
			ThreadID      string            `json:"threadId"`
			TurnID        string            `json:"turnId"`
			CompletedAtMS int64             `json:"completedAtMs"`
		}{Item: userItem, ThreadID: threadID, TurnID: turnID, CompletedAtMS: startedAtMS}},
		syntheticNotification{JSONRPC: "2.0", Method: "item/started", Params: struct {
			Item        syntheticAgentItem `json:"item"`
			ThreadID    string             `json:"threadId"`
			TurnID      string             `json:"turnId"`
			StartedAtMS int64              `json:"startedAtMs"`
		}{Item: startedAgentItem, ThreadID: threadID, TurnID: turnID, StartedAtMS: startedAtMS}},
		syntheticNotification{JSONRPC: "2.0", Method: "item/agentMessage/delta", Params: struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}{ThreadID: threadID, TurnID: turnID, ItemID: agentItemID, Delta: feedback}},
		syntheticNotification{JSONRPC: "2.0", Method: "item/completed", Params: struct {
			Item          syntheticAgentItem `json:"item"`
			ThreadID      string             `json:"threadId"`
			TurnID        string             `json:"turnId"`
			CompletedAtMS int64              `json:"completedAtMs"`
		}{Item: completedAgentItem, ThreadID: threadID, TurnID: turnID, CompletedAtMS: startedAtMS}},
		syntheticNotification{JSONRPC: "2.0", Method: "turn/completed", Params: struct {
			ThreadID string        `json:"threadId"`
			Turn     syntheticTurn `json:"turn"`
		}{ThreadID: threadID, Turn: completedTurn}},
	}

	messages := make([][]byte, 0, len(objects))
	for _, object := range objects {
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, errors.New("encode synthetic provider turn")
		}
		messages = append(messages, encoded)
	}
	return messages, nil
}

func syntheticID(kind string) (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", errors.New("generate synthetic provider turn id")
	}
	return "cps-" + kind + "-" + hex.EncodeToString(random), nil
}
