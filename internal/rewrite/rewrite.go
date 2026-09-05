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

var modelMethods = map[string]struct{}{
	"turn/start":             {},
	"thread/settings/update": {},
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
	_, modelMethod := modelMethods[method]
	injectModel := route.Model != "" && (injectProvider || modelMethod)
	if !injectProvider && !injectModel && method != "thread/list" {
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
	}
	if injectModel {
		if err := ApplyModel(params, route.Model); err != nil {
			return nil, fmt.Errorf("method %s has invalid collaboration mode", method)
		}
	}
	if method == "thread/list" {
		params["modelProviders"] = json.RawMessage("[]")
		params["sourceKinds"] = json.RawMessage(`["cli","vscode","unknown"]`)
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

// ApplyModel replaces every model field that Codex can use for one request.
func ApplyModel(params map[string]json.RawMessage, model string) error {
	params["model"] = rawJSONString(model)
	collaborationRaw, present := params["collaborationMode"]
	if !present || !hasNonNull(collaborationRaw) {
		return nil
	}
	collaboration, err := objectParams(collaborationRaw, true)
	if err != nil {
		return err
	}
	settingsRaw, present := collaboration["settings"]
	if !present || !hasNonNull(settingsRaw) {
		return errors.New("missing collaboration settings")
	}
	settings, err := objectParams(settingsRaw, true)
	if err != nil {
		return err
	}
	settings["model"] = rawJSONString(model)
	encodedSettings, err := json.Marshal(settings)
	if err != nil {
		return errors.New("encode collaboration settings")
	}
	collaboration["settings"] = encodedSettings
	encodedCollaboration, err := json.Marshal(collaboration)
	if err != nil {
		return errors.New("encode collaboration mode")
	}
	params["collaborationMode"] = encodedCollaboration
	return nil
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
