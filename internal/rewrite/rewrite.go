// Package rewrite applies the connection's provider policy to client JSON-RPC
// messages before they reach the Codex app-server proxy.
package rewrite

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var providerMethods = map[string]struct{}{
	"thread/start":  {},
	"thread/resume": {},
	"thread/fork":   {},
}

// Line validates one JSON-RPC line and applies routing fields to target
// methods. Valid non-target messages are returned unchanged.
func Line(line []byte, provider string) ([]byte, error) {
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
	if !injectProvider && method != "thread/list" {
		return line, nil
	}

	paramsRaw, paramsPresent := message["params"]
	params, err := objectParams(paramsRaw, paramsPresent && hasNonNull(paramsRaw))
	if err != nil {
		return nil, fmt.Errorf("method %s has non-object params", method)
	}

	if injectProvider {
		value, err := json.Marshal(provider)
		if err != nil {
			return nil, errors.New("encode provider")
		}
		params["modelProvider"] = value
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

// Stream rewrites newline-delimited JSON from src to dst. It deliberately uses
// Reader.ReadBytes instead of Scanner so request size is limited by memory, not
// Scanner's 64 KiB token limit.
func Stream(dst io.Writer, src io.Reader, provider string) error {
	reader := bufio.NewReader(src)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			return nil
		}

		body, ending := splitLineEnding(line)
		rewritten, err := Line(body, provider)
		if err != nil {
			return fmt.Errorf("input line %d: %w", lineNumber, err)
		}
		if err := writeFull(dst, rewritten); err != nil {
			return fmt.Errorf("write input line %d: %w", lineNumber, err)
		}
		if err := writeFull(dst, ending); err != nil {
			return fmt.Errorf("write input line %d ending: %w", lineNumber, err)
		}

		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read input line %d: %w", lineNumber+1, readErr)
		}
	}
}

func splitLineEnding(line []byte) ([]byte, []byte) {
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return line, nil
	}
	if len(line) >= 2 && line[len(line)-2] == '\r' {
		return line[:len(line)-2], line[len(line)-2:]
	}
	return line[:len(line)-1], line[len(line)-1:]
}

func writeFull(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
