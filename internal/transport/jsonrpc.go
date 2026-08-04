package transport

import (
	"bytes"
	"encoding/json"
	"errors"
)

type rpcKind uint8

const (
	rpcUnknown rpcKind = iota
	rpcRequest
	rpcNotification
	rpcResponse
)

type rpcMessage struct {
	kind         rpcKind
	method       string
	id           json.RawMessage
	idKey        string
	params       map[string]json.RawMessage
	result       map[string]json.RawMessage
	hasError     bool
	errorCode    int
	errorMessage string
	threadID     string
}

type appServerRPCError struct {
	code    int
	message string
}

func (*appServerRPCError) Error() string {
	return "app-server request failed"
}

func parseRPCMessage(payload []byte) (rpcMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope == nil {
		return rpcMessage{}, errors.New("invalid JSON-RPC object")
	}

	message := rpcMessage{}
	if rawID, ok := envelope["id"]; ok && hasJSONValue(rawID) {
		message.id = append(json.RawMessage(nil), rawID...)
		message.idKey = string(bytes.TrimSpace(rawID))
	}

	if rawMethod, ok := envelope["method"]; ok {
		if err := json.Unmarshal(rawMethod, &message.method); err != nil {
			return message, nil
		}
		if message.idKey == "" {
			message.kind = rpcNotification
		} else {
			message.kind = rpcRequest
		}
	} else if message.idKey != "" {
		message.kind = rpcResponse
	}

	message.params = decodeObject(envelope["params"])
	message.result = decodeObject(envelope["result"])
	if rawError, ok := envelope["error"]; ok && hasJSONValue(rawError) {
		message.hasError = true
		var rpcError struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(rawError, &rpcError) == nil {
			message.errorCode = rpcError.Code
			message.errorMessage = rpcError.Message
		}
	}
	if message.params != nil {
		_ = json.Unmarshal(message.params["threadId"], &message.threadID)
	}
	return message, nil
}

func requireThreadID(message rpcMessage) (string, error) {
	if message.threadID == "" {
		return "", errors.New("routing request requires a string thread id")
	}
	return message.threadID, nil
}

func responseThreadProvider(message rpcMessage) (string, string, bool) {
	if message.kind != rpcResponse || message.hasError || message.result == nil {
		return "", "", false
	}
	var thread map[string]json.RawMessage
	if err := json.Unmarshal(message.result["thread"], &thread); err != nil || thread == nil {
		return "", "", false
	}
	var threadID, provider string
	if json.Unmarshal(thread["id"], &threadID) != nil || threadID == "" ||
		json.Unmarshal(message.result["modelProvider"], &provider) != nil || provider == "" {
		return "", "", false
	}
	return threadID, provider, true
}

func responseThreadName(message rpcMessage) string {
	if message.kind != rpcResponse || message.hasError || message.result == nil {
		return ""
	}
	var thread map[string]json.RawMessage
	if json.Unmarshal(message.result["thread"], &thread) != nil || thread == nil {
		return ""
	}
	var name string
	_ = json.Unmarshal(thread["name"], &name)
	return name
}

func encodeRPCRequest(id, method string, params map[string]json.RawMessage) ([]byte, error) {
	envelope := struct {
		JSONRPC string                     `json:"jsonrpc"`
		ID      string                     `json:"id"`
		Method  string                     `json:"method"`
		Params  map[string]json.RawMessage `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, errors.New("encode internal JSON-RPC request")
	}
	return encoded, nil
}

func encodeRPCError(id json.RawMessage, code int, message string) ([]byte, error) {
	if len(bytes.TrimSpace(id)) == 0 {
		id = json.RawMessage("null")
	}
	envelope := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      id,
	}
	envelope.Error.Code = code
	envelope.Error.Message = message
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, errors.New("encode JSON-RPC error")
	}
	return encoded, nil
}

func decodeObject(raw json.RawMessage) map[string]json.RawMessage {
	if !hasJSONValue(raw) {
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil
	}
	return object
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}
