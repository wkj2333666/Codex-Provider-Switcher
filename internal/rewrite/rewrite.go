// Package rewrite applies the connection's provider policy to client JSON-RPC
// messages before they reach the Codex app-server proxy.
package rewrite

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
)

var providerMethods = map[string]struct{}{
	"thread/start":  {},
	"thread/resume": {},
	"thread/fork":   {},
}

// Line validates one JSON-RPC line and applies routing fields to target
// methods. Valid non-target messages are returned unchanged.
func Line(line []byte, route modelroute.Route) ([]byte, error) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal(line, &message); err != nil || message == nil {
		return nil, errors.New("invalid JSON object")
	}

	var method string
	methodRaw, ok := message["method"]
	if !ok || json.Unmarshal(methodRaw, &method) != nil {
		return line, nil
	}

	_, injectProvider := providerMethods[method]
	injectTurnModel := method == "turn/start" && route.Model != ""
	if !injectProvider && !injectTurnModel && method != "thread/list" {
		return line, nil
	}
	if injectProvider && route.Provider == "" {
		return line, nil
	}

	paramsRaw, paramsPresent := message["params"]
	params, err := objectParams(paramsRaw, paramsPresent && hasNonNull(paramsRaw))
	if err != nil {
		return nil, fmt.Errorf("method %s has non-object params", method)
	}

	if injectProvider {
		value, err := json.Marshal(route.Provider)
		if err != nil {
			return nil, errors.New("encode provider")
		}
		params["modelProvider"] = value
		if route.Model != "" {
			params["model"] = rawJSONString(route.Model)
		}
	} else if injectTurnModel {
		params["model"] = rawJSONString(route.Model)
	} else {
		params["modelProviders"] = json.RawMessage("[]")
	}

	encodedParams, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode params for method %s", method)
	}
	message["params"] = encodedParams

	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode method %s", method)
	}
	return encoded, nil
}

func rawJSONString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func objectParams(raw json.RawMessage, present bool) (map[string]json.RawMessage, error) {
	if !present {
		return make(map[string]json.RawMessage), nil
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil || params == nil {
		return nil, errors.New("params must be an object")
	}
	return params, nil
}

func hasNonNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}
