package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
)

const (
	maxRecoveryThreads        = 64
	freshRolloutWaitTimeout   = 15 * time.Second
	freshRolloutRetryInterval = 100 * time.Millisecond
)

type recoveryClient struct {
	connection *websocket.Conn
	sequence   uint64
}

type recoveryThread struct {
	ID     string
	Status string
}

func alreadyUnarchivedError(err error, threadID string) bool {
	var rpcError *appServerRPCError
	return errors.As(err, &rpcError) && rpcError.code == -32600 &&
		rpcError.message == "no archived rollout found for thread id "+threadID
}

func rolloutNotReadyError(err error, threadID string) bool {
	var rpcError *appServerRPCError
	return errors.As(err, &rpcError) && rpcError.code == -32600 &&
		(rpcError.message == "no rollout found" ||
			rpcError.message == "no rollout found for thread id "+threadID)
}

func retryFreshRollout(ctx context.Context, enabled bool, threadID string, operation func(context.Context) error) error {
	if !enabled {
		return operation(ctx)
	}
	waitCtx, cancel := context.WithTimeout(ctx, freshRolloutWaitTimeout)
	defer cancel()
	for {
		err := operation(waitCtx)
		if err == nil || !rolloutNotReadyError(err, threadID) {
			return err
		}
		timer := time.NewTimer(freshRolloutRetryInterval)
		select {
		case <-timer.C:
		case <-waitCtx.Done():
			timer.Stop()
			return waitCtx.Err()
		}
	}
}

func newRecoveryClient(ctx context.Context, socket string) (*recoveryClient, error) {
	connectCtx, cancel := context.WithTimeout(ctx, internalCallTimeout)
	defer cancel()
	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(dialContext context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(dialContext, "unix", socket)
		},
	}
	connection, _, err := websocket.Dial(connectCtx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: transport},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, errors.New("connect recovery client")
	}
	connection.SetReadLimit(maxMessageSize)
	client := &recoveryClient{connection: connection}
	initialize := map[string]json.RawMessage{
		"clientInfo": jsonRaw(map[string]any{
			"name": "codex_provider_switcher_recovery", "title": "Codex Provider Switcher Recovery", "version": "1",
		}),
		"capabilities": jsonRaw(map[string]any{"experimentalApi": true}),
	}
	if _, err := client.call(connectCtx, "initialize", initialize); err != nil {
		client.close()
		return nil, err
	}
	initialized := []byte(`{"jsonrpc":"2.0","method":"initialized","params":{}}`)
	if err := connection.Write(connectCtx, websocket.MessageText, initialized); err != nil {
		client.close()
		return nil, errors.New("initialize recovery client")
	}
	return client, nil
}

func (client *recoveryClient) close() {
	if client != nil && client.connection != nil {
		_ = client.connection.CloseNow()
	}
}

func (client *recoveryClient) inspectRecoverableSubtree(ctx context.Context, rootID string) ([]string, string, error) {
	thread, err := client.readThread(ctx, rootID)
	if err != nil || (thread.Status != "idle" && thread.Status != "systemError") {
		return nil, "", errors.New("recovery requires quiescent thread")
	}
	status := thread.Status

	type listedThread struct {
		ID       string  `json:"id"`
		ParentID *string `json:"parentThreadId"`
	}
	parents := make(map[string]string)
	seenCursors := make(map[string]bool)
	cursor := ""
	for {
		params := map[string]json.RawMessage{
			"ancestorThreadId": rawJSONString(rootID),
			"limit":            json.RawMessage("64"),
		}
		if cursor != "" {
			params["cursor"] = rawJSONString(cursor)
		}
		response, err := client.call(ctx, "thread/list", params)
		if err != nil {
			return nil, "", errors.New("enumerate recovery subtree")
		}
		var page struct {
			Data       []listedThread `json:"data"`
			NextCursor *string        `json:"nextCursor"`
		}
		encoded, _ := json.Marshal(response.result)
		if json.Unmarshal(encoded, &page) != nil || page.Data == nil {
			return nil, "", errors.New("invalid recovery subtree")
		}
		for _, thread := range page.Data {
			if thread.ID == "" || thread.ID == rootID || thread.ParentID == nil || *thread.ParentID == "" {
				return nil, "", errors.New("invalid recovery subtree")
			}
			if _, exists := parents[thread.ID]; exists {
				return nil, "", errors.New("duplicate recovery thread")
			}
			parents[thread.ID] = *thread.ParentID
			if len(parents)+1 > maxRecoveryThreads {
				return nil, "", errors.New("recovery subtree exceeds limit")
			}
		}
		if page.NextCursor == nil {
			break
		}
		if *page.NextCursor == "" || seenCursors[*page.NextCursor] {
			return nil, "", errors.New("invalid recovery cursor")
		}
		cursor = *page.NextCursor
		seenCursors[cursor] = true
	}

	depths := make(map[string]int, len(parents))
	visiting := make(map[string]bool, len(parents))
	var depth func(string) (int, error)
	depth = func(id string) (int, error) {
		if value, ok := depths[id]; ok {
			return value, nil
		}
		if visiting[id] {
			return 0, errors.New("cyclic recovery subtree")
		}
		visiting[id] = true
		parent := parents[id]
		value := 1
		if parent != rootID {
			if _, ok := parents[parent]; !ok {
				return 0, errors.New("incomplete recovery subtree")
			}
			parentDepth, err := depth(parent)
			if err != nil {
				return 0, err
			}
			value = parentDepth + 1
		}
		visiting[id] = false
		depths[id] = value
		return value, nil
	}
	ids := make([]string, 0, len(parents)+1)
	for id := range parents {
		if _, err := depth(id); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool {
		if depths[ids[left]] != depths[ids[right]] {
			return depths[ids[left]] > depths[ids[right]]
		}
		return ids[left] < ids[right]
	})
	return append(ids, rootID), status, nil
}

func (client *recoveryClient) readThread(ctx context.Context, threadID string) (recoveryThread, error) {
	response, err := client.call(ctx, "thread/read", map[string]json.RawMessage{
		"threadId":     rawJSONString(threadID),
		"includeTurns": json.RawMessage("false"),
	})
	if err != nil {
		return recoveryThread{}, errors.New("read recovery thread")
	}
	var result struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	encoded, _ := json.Marshal(response.result)
	if json.Unmarshal(encoded, &result) != nil || result.Thread.ID != threadID {
		return recoveryThread{}, errors.New("invalid recovery thread")
	}
	return recoveryThread{ID: result.Thread.ID, Status: result.Thread.Status.Type}, nil
}

func (client *recoveryClient) archive(ctx context.Context, threadID string, waitForFreshRollout bool) error {
	err := retryFreshRollout(ctx, waitForFreshRollout, threadID, func(callCtx context.Context) error {
		_, callErr := client.call(callCtx, "thread/archive", map[string]json.RawMessage{
			"threadId": rawJSONString(threadID),
		})
		return callErr
	})
	if err != nil {
		return errors.New("archive recovery thread")
	}
	return nil
}

func (client *recoveryClient) unarchive(ctx context.Context, threadID string) error {
	response, err := client.call(ctx, "thread/unarchive", map[string]json.RawMessage{"threadId": rawJSONString(threadID)})
	if err != nil {
		return fmt.Errorf("unarchive recovery thread: %w", err)
	}
	var thread struct {
		ID string `json:"id"`
	}
	if encoded, marshalErr := json.Marshal(response.result["thread"]); marshalErr != nil ||
		json.Unmarshal(encoded, &thread) != nil || thread.ID != threadID {
		return errors.New("verify unarchived recovery thread")
	}
	return nil
}

func (client *recoveryClient) resume(ctx context.Context, threadID string, expectedRoute modelroute.Route) (modelroute.Route, error) {
	params := map[string]json.RawMessage{
		"threadId":      rawJSONString(threadID),
		"modelProvider": rawJSONString(expectedRoute.Provider),
		// Recovery only needs runtime verification. Full-turn hydration can
		// make very large paginated threads fail or time out.
		"excludeTurns": json.RawMessage("true"),
	}
	if expectedRoute.Model != "" {
		params["model"] = rawJSONString(expectedRoute.Model)
	}
	response, err := client.call(ctx, "thread/resume", params)
	if err != nil {
		return modelroute.Route{}, errors.New("resume recovery thread")
	}
	responseThreadID, route, ok := responseThreadRoute(response)
	if !ok || responseThreadID != threadID || !routeMatches(route, expectedRoute) {
		return modelroute.Route{}, errors.New("verify resumed recovery thread")
	}
	return route, nil
}

func (client *recoveryClient) unsubscribe(ctx context.Context, threadID string) error {
	response, err := client.call(ctx, "thread/unsubscribe", map[string]json.RawMessage{
		"threadId": rawJSONString(threadID),
	})
	if err != nil {
		return errors.New("unsubscribe recovery thread")
	}
	var status string
	if json.Unmarshal(response.result["status"], &status) != nil ||
		(status != "unsubscribed" && status != "notSubscribed" && status != "notLoaded") {
		return errors.New("verify recovery unsubscribe")
	}
	return nil
}

func (client *recoveryClient) call(ctx context.Context, method string, params map[string]json.RawMessage) (rpcMessage, error) {
	callCtx, cancel := context.WithTimeout(ctx, internalCallTimeout)
	defer cancel()
	client.sequence++
	id := "recovery-" + strconv.FormatUint(client.sequence, 10)
	payload, err := encodeRPCRequest(id, method, params)
	if err != nil {
		return rpcMessage{}, err
	}
	if err := client.connection.Write(callCtx, websocket.MessageText, payload); err != nil {
		return rpcMessage{}, errors.New("write recovery request")
	}
	wantID := string(rawJSONString(id))
	for {
		messageType, payload, err := client.connection.Read(callCtx)
		if err != nil {
			return rpcMessage{}, errors.New("read recovery response")
		}
		if messageType != websocket.MessageText {
			continue
		}
		message, err := parseRPCMessage(payload)
		if err != nil {
			return rpcMessage{}, errors.New("decode recovery response")
		}
		if message.kind != rpcResponse || message.idKey != wantID {
			continue
		}
		if message.hasError {
			var envelope struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(payload, &envelope) != nil || envelope.Error.Message == "" {
				return rpcMessage{}, errors.New("recovery app-server request failed")
			}
			return rpcMessage{}, &appServerRPCError{code: envelope.Error.Code, message: envelope.Error.Message}
		}
		if message.result == nil {
			return rpcMessage{}, errors.New("recovery app-server request failed")
		}
		return message, nil
	}
}

func jsonRaw(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
