package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/handoff"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/rewrite"
)

const (
	handoffErrorCode          = -32090
	handoffUnavailableMessage = "provider handoff unavailable; retry after the active turn finishes"
	internalCallTimeout       = 30 * time.Second
	handoffRecoveryTimeout    = 5 * time.Second
)

type handoffCoordinator interface {
	LockThread(context.Context, string) (func(), error)
	PrepareAll(context.Context, string) error
	PrepareHandoffAll(context.Context, string) error
	UnsubscribeAll(context.Context, string) error
	ResubscribeAll(context.Context, string, string) error
	RestoreAll(context.Context, string) error
	MarkDirty(string) error
	IsDirty(string) bool
	ClearDirty(string) error
	Close() error
}

type websocketWriteFunc func(context.Context, websocket.MessageType, []byte) error

type desktopRequest struct {
	method       string
	threadID     string
	responseSeen chan struct{}
}

type session struct {
	provider        string
	appServerSocket string

	upstreamWrite   websocketWriteFunc
	downstreamWrite websocketWriteFunc

	upstreamWriteMu   sync.Mutex
	downstreamWriteMu sync.Mutex
	stateMu           sync.Mutex
	internal          map[string]chan rpcMessage
	desktop           map[string]*desktopRequest
	resumeTemplates   map[string]map[string]json.RawMessage
	effective         map[string]string
	detached          map[string]bool
	active            map[string]bool
	internalPrefix    string
	sequence          atomic.Uint64
	closed            chan struct{}
	closeOnce         sync.Once
	coordinator       handoffCoordinator
}

func newSessionState(provider, appServerSocket string, upstreamWrite, downstreamWrite websocketWriteFunc) (*session, error) {
	if provider == "" || appServerSocket == "" || upstreamWrite == nil || downstreamWrite == nil {
		return nil, errors.New("invalid provider handoff session")
	}
	prefixBytes := make([]byte, 8)
	if _, err := rand.Read(prefixBytes); err != nil {
		return nil, errors.New("generate internal request prefix")
	}
	return &session{
		provider:        provider,
		appServerSocket: appServerSocket,
		upstreamWrite:   upstreamWrite,
		downstreamWrite: downstreamWrite,
		internal:        make(map[string]chan rpcMessage),
		desktop:         make(map[string]*desktopRequest),
		resumeTemplates: make(map[string]map[string]json.RawMessage),
		effective:       make(map[string]string),
		detached:        make(map[string]bool),
		active:          make(map[string]bool),
		internalPrefix:  "cps-" + hex.EncodeToString(prefixBytes),
		closed:          make(chan struct{}),
	}, nil
}

func (current *session) handleDownstreamText(ctx context.Context, payload []byte) error {
	rewritten, err := rewrite.Line(payload, current.provider)
	if err != nil {
		return errRoutingPolicy
	}
	message, err := parseRPCMessage(rewritten)
	if err != nil {
		return errRoutingPolicy
	}

	switch message.method {
	case "turn/start":
		return current.handleTurnStart(ctx, message, rewritten)
	case "thread/resume":
		return current.handleThreadResume(ctx, message, rewritten)
	case "thread/unsubscribe":
		return current.handleThreadUnsubscribe(ctx, message, rewritten)
	case "thread/start", "thread/fork":
		current.trackDesktopRequest(message, &desktopRequest{method: message.method})
	}
	return current.writeUpstream(ctx, websocket.MessageText, rewritten)
}

func (current *session) handleThreadUnsubscribe(ctx context.Context, message rpcMessage, payload []byte) error {
	threadID, err := requireThreadID(message)
	if err != nil || message.idKey == "" || current.coordinator == nil {
		return errRoutingPolicy
	}
	release, err := current.coordinator.LockThread(ctx, threadID)
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	defer release()

	seen := make(chan struct{})
	current.trackDesktopRequest(message, &desktopRequest{method: message.method, threadID: threadID, responseSeen: seen})
	current.clearEffectiveProvider(threadID)
	current.setDetached(threadID, false)
	if err := current.writeUpstream(ctx, websocket.MessageText, payload); err != nil {
		current.removeDesktopRequest(message.idKey)
		return err
	}
	select {
	case <-seen:
		return nil
	case <-ctx.Done():
		current.removeDesktopRequest(message.idKey)
		return ctx.Err()
	case <-current.closed:
		return nil
	}
}

func (current *session) handleTurnStart(ctx context.Context, message rpcMessage, payload []byte) error {
	threadID, err := requireThreadID(message)
	if err != nil || message.idKey == "" || current.coordinator == nil {
		return errRoutingPolicy
	}
	release, err := current.coordinator.LockThread(ctx, threadID)
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	defer release()

	if current.isActive(threadID) {
		return current.writeHandoffError(ctx, message.id)
	}
	if err := current.coordinator.PrepareAll(ctx, threadID); err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	if current.coordinator.IsDirty(threadID) || current.effectiveProvider(threadID) != current.provider {
		if err := current.handoff(ctx, threadID); err != nil {
			return current.writeHandoffError(ctx, message.id)
		}
	}

	current.setActive(threadID, true)
	current.trackDesktopRequest(message, &desktopRequest{method: message.method, threadID: threadID})
	if err := current.writeUpstream(ctx, websocket.MessageText, payload); err != nil {
		current.removeDesktopRequest(message.idKey)
		current.setActive(threadID, false)
		return err
	}
	return nil
}

func (current *session) handleThreadResume(ctx context.Context, message rpcMessage, payload []byte) error {
	threadID, err := requireThreadID(message)
	if err != nil || message.idKey == "" || current.coordinator == nil || message.params == nil {
		return errRoutingPolicy
	}
	current.stateMu.Lock()
	current.resumeTemplates[threadID] = cloneRawMap(message.params)
	current.stateMu.Unlock()

	release, err := current.coordinator.LockThread(ctx, threadID)
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	defer release()

	seen := make(chan struct{})
	current.trackDesktopRequest(message, &desktopRequest{method: message.method, threadID: threadID, responseSeen: seen})
	current.clearEffectiveProvider(threadID)
	current.setDetached(threadID, false)
	if err := current.writeUpstream(ctx, websocket.MessageText, payload); err != nil {
		current.removeDesktopRequest(message.idKey)
		return err
	}
	select {
	case <-seen:
		return nil
	case <-ctx.Done():
		current.removeDesktopRequest(message.idKey)
		return ctx.Err()
	case <-current.closed:
		return nil
	}
}

func (current *session) handoff(ctx context.Context, threadID string) error {
	if err := current.coordinator.PrepareHandoffAll(ctx, threadID); err != nil {
		return errors.New("provider handoff capability check failed")
	}
	if err := current.coordinator.MarkDirty(threadID); err != nil {
		return errors.New("provider handoff dirty marker failed")
	}
	if err := current.coordinator.UnsubscribeAll(ctx, threadID); err != nil {
		current.restoreAfterHandoffFailure(threadID)
		return errors.New("provider handoff unsubscribe failed")
	}
	if err := current.internalResume(ctx, threadID); err != nil {
		current.restoreAfterHandoffFailure(threadID)
		return err
	}
	if err := current.coordinator.ResubscribeAll(ctx, threadID, current.provider); err != nil {
		current.clearEffectiveProvider(threadID)
		current.restoreAfterHandoffFailure(threadID)
		return errors.New("provider handoff resubscribe failed")
	}
	if err := current.coordinator.ClearDirty(threadID); err != nil {
		current.restoreAfterHandoffFailure(threadID)
		return errors.New("provider handoff dirty marker clear failed")
	}
	return nil
}

func (current *session) restoreAfterHandoffFailure(threadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), handoffRecoveryTimeout)
	defer cancel()
	_ = current.coordinator.RestoreAll(ctx, threadID)
}

func (current *session) internalResume(ctx context.Context, threadID string) error {
	return current.internalResumeWithProvider(ctx, threadID, current.provider, true)
}

func (current *session) internalResumeWithProvider(ctx context.Context, threadID, expectedProvider string, reuseTemplate bool) error {
	var params map[string]json.RawMessage
	if reuseTemplate {
		current.stateMu.Lock()
		params = cloneRawMap(current.resumeTemplates[threadID])
		current.stateMu.Unlock()
	}
	if params == nil {
		params = make(map[string]json.RawMessage)
	}
	params["threadId"] = rawJSONString(threadID)
	params["modelProvider"] = rawJSONString(expectedProvider)

	response, err := current.callUpstream(ctx, "thread/resume", params)
	if err != nil {
		return err
	}
	responseThreadID, provider, ok := responseThreadProvider(response)
	if !ok || responseThreadID != threadID || provider != expectedProvider {
		return errors.New("provider handoff verification failed")
	}
	current.stateMu.Lock()
	current.effective[threadID] = provider
	delete(current.detached, threadID)
	current.stateMu.Unlock()
	return nil
}

func (current *session) callUpstream(ctx context.Context, method string, params map[string]json.RawMessage) (rpcMessage, error) {
	callCtx, cancel := context.WithTimeout(ctx, internalCallTimeout)
	defer cancel()
	id := current.internalPrefix + "-" + strconv.FormatUint(current.sequence.Add(1), 10)
	idRaw := rawJSONString(id)
	idKey := string(idRaw)
	waiter := make(chan rpcMessage, 1)
	current.stateMu.Lock()
	current.internal[idKey] = waiter
	current.stateMu.Unlock()

	payload, err := encodeRPCRequest(id, method, params)
	if err != nil {
		current.removeInternalRequest(idKey)
		return rpcMessage{}, err
	}
	if err := current.writeUpstream(callCtx, websocket.MessageText, payload); err != nil {
		current.abandonInternalRequest(idKey)
		return rpcMessage{}, err
	}
	select {
	case response := <-waiter:
		if response.hasError {
			return rpcMessage{}, errors.New("internal app-server request failed")
		}
		return response, nil
	case <-callCtx.Done():
		current.abandonInternalRequest(idKey)
		return rpcMessage{}, callCtx.Err()
	case <-current.closed:
		current.abandonInternalRequest(idKey)
		return rpcMessage{}, errors.New("provider handoff session closed")
	}
}

// Unsubscribe implements handoff.Handler for this app-server connection.
func (current *session) Unsubscribe(ctx context.Context, threadID string) (handoff.PeerStatus, error) {
	if current.isActive(threadID) {
		return handoff.StatusBusy, nil
	}
	wasSubscribed := current.effectiveProvider(threadID) != "" || current.isDetached(threadID)
	response, err := current.callUpstream(ctx, "thread/unsubscribe", map[string]json.RawMessage{
		"threadId": rawJSONString(threadID),
	})
	current.clearEffectiveProvider(threadID)
	if wasSubscribed {
		current.setDetached(threadID, true)
	}
	if err != nil || response.result == nil {
		return "", errors.New("unsubscribe app-server thread")
	}
	var status handoff.PeerStatus
	if json.Unmarshal(response.result["status"], &status) != nil {
		return "", errors.New("invalid thread unsubscribe response")
	}
	if status != handoff.StatusUnsubscribed && status != handoff.StatusNotSubscribed && status != handoff.StatusNotLoaded {
		return "", errors.New("unsupported thread unsubscribe status")
	}
	if status == handoff.StatusUnsubscribed {
		current.setDetached(threadID, true)
	}
	return status, nil
}

// Resubscribe implements handoff.Handler for a connection detached during the
// coordinated provider transition.
func (current *session) Resubscribe(ctx context.Context, threadID, provider string) (handoff.PeerStatus, error) {
	if !current.isDetached(threadID) {
		return handoff.StatusNotSubscribed, nil
	}
	if provider == "" {
		return "", errors.New("invalid provider handoff resubscribe")
	}
	if err := current.internalResumeWithProvider(ctx, threadID, provider, false); err != nil {
		return "", errors.New("resubscribe app-server thread")
	}
	return handoff.StatusResubscribed, nil
}

// Restore implements handoff.Handler by reattaching without requesting a
// provider change.
func (current *session) Restore(ctx context.Context, threadID string) (handoff.PeerStatus, error) {
	if !current.isDetached(threadID) {
		return handoff.StatusNotSubscribed, nil
	}
	response, err := current.callUpstream(ctx, "thread/resume", map[string]json.RawMessage{
		"threadId": rawJSONString(threadID),
	})
	if err != nil {
		return "", errors.New("restore app-server thread")
	}
	responseThreadID, provider, ok := responseThreadProvider(response)
	if !ok || responseThreadID != threadID {
		return "", errors.New("verify restored app-server thread")
	}
	current.stateMu.Lock()
	current.effective[threadID] = provider
	delete(current.detached, threadID)
	current.stateMu.Unlock()
	return handoff.StatusRestored, nil
}

func (current *session) writeHandoffError(ctx context.Context, id json.RawMessage) error {
	payload, err := encodeRPCError(id, handoffErrorCode, handoffUnavailableMessage)
	if err != nil {
		return err
	}
	return current.writeDownstream(ctx, websocket.MessageText, payload)
}

func (current *session) trackDesktopRequest(message rpcMessage, request *desktopRequest) {
	if message.idKey == "" {
		return
	}
	current.stateMu.Lock()
	current.desktop[message.idKey] = request
	current.stateMu.Unlock()
}

func (current *session) removeDesktopRequest(idKey string) {
	current.stateMu.Lock()
	delete(current.desktop, idKey)
	current.stateMu.Unlock()
}

func (current *session) removeInternalRequest(idKey string) {
	current.stateMu.Lock()
	delete(current.internal, idKey)
	current.stateMu.Unlock()
}

func (current *session) abandonInternalRequest(idKey string) {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	if _, ok := current.internal[idKey]; ok {
		current.internal[idKey] = nil
	}
}

func cloneRawMap(source map[string]json.RawMessage) map[string]json.RawMessage {
	if source == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		result[key] = append(json.RawMessage(nil), value...)
	}
	return result
}

func rawJSONString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func (current *session) handleUpstreamText(ctx context.Context, payload []byte) error {
	message, err := parseRPCMessage(payload)
	if err != nil {
		return current.writeDownstream(ctx, websocket.MessageText, payload)
	}

	if message.kind == rpcResponse {
		current.stateMu.Lock()
		if waiter, ok := current.internal[message.idKey]; ok {
			delete(current.internal, message.idKey)
			current.stateMu.Unlock()
			if waiter != nil {
				waiter <- message
			}
			return nil
		}

		request := current.desktop[message.idKey]
		if request != nil {
			delete(current.desktop, message.idKey)
			if threadID, provider, ok := responseThreadProvider(message); ok &&
				(request.method == "thread/start" || request.method == "thread/resume" || request.method == "thread/fork") {
				current.effective[threadID] = provider
				delete(current.detached, threadID)
			}
			if request.method == "turn/start" && message.hasError {
				delete(current.active, request.threadID)
			}
			if request.method == "thread/unsubscribe" && !message.hasError {
				delete(current.effective, request.threadID)
				delete(current.detached, request.threadID)
			}
		}
		current.stateMu.Unlock()
		if request != nil && request.responseSeen != nil {
			close(request.responseSeen)
		}
	}

	if message.kind == rpcNotification {
		switch message.method {
		case "turn/started":
			if message.threadID != "" {
				current.setActive(message.threadID, true)
			}
		case "turn/completed":
			if message.threadID != "" {
				current.setActive(message.threadID, false)
			}
		case "thread/closed":
			if message.threadID != "" {
				current.clearThreadRoutingState(message.threadID)
			}
		}
	}

	return current.writeDownstream(ctx, websocket.MessageText, payload)
}

func (current *session) pumpDownstream(ctx context.Context, source *websocket.Conn) error {
	for {
		messageType, payload, err := source.Read(ctx)
		if err != nil {
			return err
		}
		if messageType == websocket.MessageText {
			if err := current.handleDownstreamText(ctx, payload); err != nil {
				return err
			}
			continue
		}
		if err := current.writeUpstream(ctx, messageType, payload); err != nil {
			return err
		}
	}
}

func (current *session) pumpUpstream(ctx context.Context, source *websocket.Conn) error {
	for {
		messageType, payload, err := source.Read(ctx)
		if err != nil {
			return err
		}
		if messageType == websocket.MessageText {
			if err := current.handleUpstreamText(ctx, payload); err != nil {
				return err
			}
			continue
		}
		if err := current.writeDownstream(ctx, messageType, payload); err != nil {
			return err
		}
	}
}

func (current *session) writeUpstream(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
	current.upstreamWriteMu.Lock()
	defer current.upstreamWriteMu.Unlock()
	return current.upstreamWrite(ctx, messageType, payload)
}

func (current *session) writeDownstream(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
	current.downstreamWriteMu.Lock()
	defer current.downstreamWriteMu.Unlock()
	return current.downstreamWrite(ctx, messageType, payload)
}

func (current *session) effectiveProvider(threadID string) string {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	return current.effective[threadID]
}

func (current *session) clearEffectiveProvider(threadID string) {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	delete(current.effective, threadID)
}

func (current *session) clearThreadRoutingState(threadID string) {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	delete(current.effective, threadID)
	delete(current.detached, threadID)
}

func (current *session) setDetached(threadID string, detached bool) {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	if detached {
		current.detached[threadID] = true
	} else {
		delete(current.detached, threadID)
	}
}

func (current *session) isDetached(threadID string) bool {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	return current.detached[threadID]
}

func (current *session) setActive(threadID string, active bool) {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	if active {
		current.active[threadID] = true
	} else {
		delete(current.active, threadID)
	}
}

func (current *session) isActive(threadID string) bool {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	return current.active[threadID]
}

func (current *session) Prepare(threadID string) handoff.PeerStatus {
	if current.isActive(threadID) {
		return handoff.StatusBusy
	}
	return handoff.StatusReady
}

func (current *session) closeState() {
	current.closeOnce.Do(func() {
		current.stateMu.Lock()
		requests := current.desktop
		current.desktop = make(map[string]*desktopRequest)
		current.internal = make(map[string]chan rpcMessage)
		current.stateMu.Unlock()
		for _, request := range requests {
			if request.responseSeen != nil {
				close(request.responseSeen)
			}
		}
		close(current.closed)
	})
}
