package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/handoff"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
	providerid "github.com/wkj2333666/Codex-Provider-Switcher/internal/provider"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/recovery"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/rewrite"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/rollout"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/selection"
)

const (
	handoffErrorCode          = -32090
	handoffUnavailableMessage = "provider handoff unavailable; retry or check history restoration and task activity"
	internalCallTimeout       = 30 * time.Second
	handoffRecoveryTimeout    = 5 * time.Second
	providerCommandErrorCode  = -32602
	maxRolloutListPages       = 128
	rolloutListPageSize       = 100
)

var errProviderMismatch = errors.New("provider handoff verification mismatch")

type handoffCoordinator interface {
	LockThread(context.Context, string) (func(), error)
	PrepareAll(context.Context, string) error
	PrepareHandoffAll(context.Context, string) error
	PrepareRecoveryAll(context.Context, string) error
	ReconcileIdleAll(context.Context, string) error
	UnsubscribeAll(context.Context, string) error
	ResubscribeAll(context.Context, string, modelroute.Route) error
	RestoreAll(context.Context, string) error
	MarkDirty(string) error
	SetDirtyStage(string, handoff.DirtyStage) error
	IsDirty(string) bool
	ReadDirtyStage(string) (handoff.DirtyStage, bool, error)
	ClearDirty(string) error
	BeginRecoveryAll(context.Context, string, []string) error
	EndRecoveryAll(context.Context, string) error
	Close() error
}

type websocketWriteFunc func(context.Context, websocket.MessageType, []byte) error

type providerSelections interface {
	GetRoute(string) (selection.Value, bool, error)
	SetRoute(string, selection.Value) error
}

type recoveryJournals interface {
	Load(string) (recovery.Journal, bool, error)
	Save(recovery.Journal) error
	Clear(string) error
}

type desktopRequest struct {
	method        string
	threadID      string
	targetRoute   modelroute.Route
	responseSeen  chan struct{}
	responseOK    bool
	releaseThread func()
}

type session struct {
	provider        string
	codexHome       string
	routes          *modelroute.Catalog
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
	effectiveModel    map[string]string
	fresh             map[string]string
	detached          map[string]bool
	active            map[string]bool
	recovery          map[string]map[string]bool
	internalPrefix    string
	sequence          atomic.Uint64
	closed            chan struct{}
	closeOnce         sync.Once
	coordinator       handoffCoordinator
	selections        providerSelections
	recoveries        recoveryJournals
	repairForkPreview func(context.Context, string, string) (string, error)
}

func newSessionState(provider string, routes *modelroute.Catalog, appServerSocket string, upstreamWrite, downstreamWrite websocketWriteFunc) (*session, error) {
	if appServerSocket == "" || upstreamWrite == nil || downstreamWrite == nil {
		return nil, errors.New("invalid provider handoff session")
	}
	prefixBytes := make([]byte, 8)
	if _, err := rand.Read(prefixBytes); err != nil {
		return nil, errors.New("generate internal request prefix")
	}
	return &session{
		provider:          provider,
		routes:            routes,
		appServerSocket:   appServerSocket,
		upstreamWrite:     upstreamWrite,
		downstreamWrite:   downstreamWrite,
		internal:          make(map[string]chan rpcMessage),
		desktop:           make(map[string]*desktopRequest),
		resumeTemplates:   make(map[string]map[string]json.RawMessage),
		effective:         make(map[string]string),
		effectiveModel:    make(map[string]string),
		fresh:             make(map[string]string),
		detached:          make(map[string]bool),
		active:            make(map[string]bool),
		recovery:          make(map[string]map[string]bool),
		internalPrefix:    "cps-" + hex.EncodeToString(prefixBytes),
		closed:            make(chan struct{}),
		repairForkPreview: rollout.RepairForkPreview,
	}, nil
}

func (current *session) handleDownstreamText(ctx context.Context, payload []byte) error {
	message, err := parseRPCMessage(payload)
	if err != nil {
		return errRoutingPolicy
	}

	if message.kind == rpcRequest {
		if threadID, err := requireThreadID(message); err == nil {
			rollout.NoteAccess(current.codexHome, threadID)
		}
	}

	switch message.method {
	case "turn/start":
		return current.handleTurnStart(ctx, message, payload)
	case "thread/settings/update":
		return current.handleThreadSettingsUpdate(ctx, message, payload)
	case "thread/resume":
		return current.handleThreadResume(ctx, message, payload)
	case "thread/unsubscribe":
		return current.handleThreadUnsubscribe(ctx, message, payload)
	}

	rewritten, err := rewrite.Line(payload, current.routeForProvider(current.provider))
	if err != nil {
		return errRoutingPolicy
	}
	if message.kind == rpcRequest {
		if !current.trackDesktopRequest(message, &desktopRequest{method: message.method}) {
			return current.writeHandoffError(ctx, message.id)
		}
		if err := current.writeUpstream(ctx, websocket.MessageText, rewritten); err != nil {
			current.removeDesktopRequest(message.idKey)
			return err
		}
		return nil
	}
	return current.writeUpstream(ctx, websocket.MessageText, rewritten)
}

func (current *session) handleThreadSettingsUpdate(ctx context.Context, message rpcMessage, payload []byte) error {
	threadID, err := requireThreadID(message)
	if err != nil || message.idKey == "" || current.coordinator == nil || message.params == nil {
		return errRoutingPolicy
	}
	seen := make(chan struct{})
	request := &desktopRequest{method: message.method, threadID: threadID, responseSeen: seen}
	if !current.trackDesktopRequest(message, request) {
		return current.writeHandoffError(ctx, message.id)
	}
	forwarded := false
	defer func() {
		if !forwarded {
			current.removeDesktopRequest(message.idKey)
		}
	}()
	release, err := current.coordinator.LockThread(ctx, threadID)
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	defer release()

	targetRoute, selected, err := current.selectedRoute(threadID)
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	if !selected {
		err := current.writeUpstream(ctx, websocket.MessageText, payload)
		forwarded = err == nil
		return err
	}
	persistRoute := false
	requested, present, err := requestedModel(message.params)
	if err != nil {
		return errRoutingPolicy
	}
	if present {
		if route, allowed := current.routes.ResolveModel(targetRoute.Provider, requested); allowed {
			targetRoute = route
			persistRoute = true
		}
	}
	rewritten, err := rewrite.Line(payload, targetRoute)
	if err != nil {
		return errRoutingPolicy
	}
	if !persistRoute {
		err := current.writeUpstream(ctx, websocket.MessageText, rewritten)
		forwarded = err == nil
		return err
	}
	if err := current.writeUpstream(ctx, websocket.MessageText, rewritten); err != nil {
		return err
	}
	forwarded = true
	select {
	case <-seen:
		if !request.responseOK {
			return nil
		}
		if err := current.persistSelectedRoute(threadID, targetRoute); err != nil {
			return errRoutingPolicy
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-current.closed:
		return nil
	}
}

func (current *session) handleThreadUnsubscribe(ctx context.Context, message rpcMessage, payload []byte) error {
	threadID, err := requireThreadID(message)
	if err != nil || message.idKey == "" || current.coordinator == nil {
		return errRoutingPolicy
	}
	seen := make(chan struct{})
	request := &desktopRequest{method: message.method, threadID: threadID, responseSeen: seen}
	if !current.trackDesktopRequest(message, request) {
		return current.writeHandoffError(ctx, message.id)
	}
	forwarded := false
	defer func() {
		if !forwarded {
			current.removeDesktopRequest(message.idKey)
		}
	}()
	release, err := current.coordinator.LockThread(ctx, threadID)
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	defer release()

	current.clearEffectiveProvider(threadID)
	current.setDetached(threadID, false)
	if err := current.writeUpstream(ctx, websocket.MessageText, payload); err != nil {
		return err
	}
	forwarded = true
	select {
	case <-seen:
		return nil
	case <-ctx.Done():
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
	request := &desktopRequest{method: message.method, threadID: threadID}
	if !current.trackDesktopRequest(message, request) {
		return current.writeHandoffError(ctx, message.id)
	}
	forwarded := false
	defer func() {
		if !forwarded {
			current.removeDesktopRequest(message.idKey)
		}
	}()
	command, commandRecognized, commandErr := parseProviderCommand(message)
	if commandErr != nil {
		return current.writeProviderCommandError(ctx, message.id)
	}
	release, err := current.coordinator.LockThread(ctx, threadID)
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	releaseOwned := true
	defer func() {
		if releaseOwned {
			release()
		}
	}()

	if err := current.prepareTurnPeers(ctx, threadID); err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	if err := current.repairRecoveryJournal(ctx, threadID); err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	if commandRecognized && command.action == providerCommandStatus {
		return current.writeProviderStatus(ctx, message.id, threadID)
	}
	targetRoute := current.routeForProvider(command.provider)
	persistRequestedRoute := false
	effectiveRoute := modelroute.Route{}
	effectiveResolved := false
	if !commandRecognized {
		var selected, exact bool
		targetRoute, selected, exact, err = current.selectedRouteState(threadID)
		if err != nil {
			return current.writeHandoffError(ctx, message.id)
		}
		if !selected {
			effectiveRoute, err = current.effectiveRouteForTurn(ctx, threadID)
			if err != nil || effectiveRoute.Provider == "" {
				return current.writeHandoffError(ctx, message.id)
			}
			effectiveResolved = true
			targetRoute = current.routeForProvider(effectiveRoute.Provider)
		}
		requested, present, requestErr := requestedModel(message.params)
		if requestErr != nil {
			return errRoutingPolicy
		}
		if present && !exact {
			if route, allowed := current.routes.ResolveModel(targetRoute.Provider, requested); allowed {
				targetRoute = route
				persistRequestedRoute = selected
			}
		}
	}
	dirty := current.coordinator.IsDirty(threadID)
	var dirtyStage handoff.DirtyStage
	var dirtyStageFound bool
	var dirtyStageErr error
	if dirty {
		dirtyStage, dirtyStageFound, dirtyStageErr = current.coordinator.ReadDirtyStage(threadID)
	}
	forceFullDirty := dirty && (commandRecognized || dirtyStageErr != nil || !dirtyStageFound ||
		dirtyStage != handoff.DirtyStageUnsubscribed)
	if !forceFullDirty && !effectiveResolved {
		// A saved selection describes the requested route, not the loaded
		// runtime. Reuse an observed route or resolve it before deciding
		// whether a handoff is needed, including for explicit switch commands.
		effectiveRoute, err = current.effectiveRouteForTurn(ctx, threadID)
		if err != nil {
			return current.writeHandoffError(ctx, message.id)
		}
	}
	if dirty {
		if !forceFullDirty && sameProvider(effectiveRoute, targetRoute) {
			if err := current.repairSameProviderDirty(ctx, threadID, targetRoute.Provider); err != nil {
				return current.writeHandoffError(ctx, message.id)
			}
		} else if err := current.handoff(ctx, threadID, targetRoute); err != nil {
			return current.writeHandoffError(ctx, message.id)
		}
	} else if (commandRecognized && !routeMatches(effectiveRoute, targetRoute)) ||
		(!commandRecognized && !sameProvider(effectiveRoute, targetRoute)) {
		if err := current.handoff(ctx, threadID, targetRoute); err != nil {
			return current.writeHandoffError(ctx, message.id)
		}
	}
	if commandRecognized {
		if current.persistSelectedRoute(threadID, targetRoute) != nil {
			return current.writeHandoffError(ctx, message.id)
		}
		feedback := "Provider switched to " + targetRoute.Provider + "."
		if targetRoute.Model != "" {
			feedback = "Provider switched to " + targetRoute.Provider + " using model " + targetRoute.Model + "."
		}
		return current.writeProviderControlTurn(
			ctx, message.id, threadID, "/provider switch "+targetRoute.Provider, feedback,
		)
	}
	if persistRequestedRoute {
		if err := current.persistSelectedRoute(threadID, targetRoute); err != nil {
			return current.writeHandoffError(ctx, message.id)
		}
	}

	rewritten, err := rewrite.Line(payload, targetRoute)
	if err != nil {
		return errRoutingPolicy
	}
	current.setActive(threadID, true)
	request.targetRoute = targetRoute
	request.releaseThread = release
	// Keep the cross-process fence until app-server acknowledges turn/start.
	// Otherwise a peer can observe an old idle status after this write and
	// mistake this genuine in-flight request for a stale active cache.
	releaseOwned = false
	if err := current.writeUpstream(ctx, websocket.MessageText, rewritten); err != nil {
		current.setActive(threadID, false)
		release()
		return err
	}
	forwarded = true
	return nil
}

func (current *session) repairSameProviderDirty(ctx context.Context, threadID, provider string) error {
	if !providerid.Valid(provider) {
		return errors.New("invalid same-provider handoff repair")
	}
	if err := current.coordinator.PrepareHandoffAll(ctx, threadID); err != nil {
		return errors.New("same-provider handoff repair capability check failed")
	}
	if err := current.coordinator.ResubscribeAll(ctx, threadID, modelroute.Route{Provider: provider}); err != nil {
		current.restoreAfterHandoffFailure(threadID)
		return errors.New("same-provider handoff repair resubscribe failed")
	}
	if err := current.coordinator.ClearDirty(threadID); err != nil {
		return errors.New("same-provider handoff repair dirty marker clear failed")
	}
	return nil
}

func (current *session) prepareTurnPeers(ctx context.Context, threadID string) error {
	if current.isActive(threadID) {
		return errors.New("provider handoff session is active")
	}
	if err := current.coordinator.PrepareAll(ctx, threadID); err == nil {
		return nil
	}

	_, _, status, err := current.findRolloutPath(ctx, threadID)
	if err != nil || (status != "idle" && status != "systemError") {
		return errors.New("provider handoff peers are active")
	}
	if err := current.coordinator.ReconcileIdleAll(ctx, threadID); err != nil {
		return errors.New("reconcile provider handoff peers")
	}
	if err := current.coordinator.PrepareAll(ctx, threadID); err != nil {
		return errors.New("provider handoff peer remained unavailable")
	}
	return nil
}

func (current *session) writeProviderStatus(ctx context.Context, id json.RawMessage, threadID string) error {
	selected, hasSelection, err := current.selectedRoute(threadID)
	if err != nil {
		return current.writeHandoffError(ctx, id)
	}
	runtime := current.effectiveRoute(threadID)
	if !providerid.Valid(runtime.Provider) {
		runtime = modelroute.Route{}
	}
	feedback := providerStatusFeedback(runtime, selected, hasSelection)
	return current.writeProviderControlTurn(ctx, id, threadID, "/provider status", feedback)
}

func providerStatusFeedback(runtime, selected modelroute.Route, hasSelection bool) string {
	runtimeLine := "Runtime provider: unknown."
	if runtime.Provider != "" {
		runtimeLine = "Runtime provider: " + runtime.Provider + " (verified)."
		if runtime.Model != "" {
			runtimeLine += "\nRuntime model: " + runtime.Model + " (verified)."
		}
	}
	if !hasSelection {
		return runtimeLine + "\nSelected provider: app-server configuration."
	}
	selectedLine := "Selected provider: " + selected.Provider + "."
	if !routeMatches(runtime, selected) {
		selectedLine = "Selected provider: " + selected.Provider + " (will be applied before the next model turn)."
	}
	if selected.Model != "" {
		selectedLine += "\nSelected model: " + selected.Model + "."
	}
	return runtimeLine + "\n" + selectedLine
}

func (current *session) writeProviderControlTurn(
	ctx context.Context,
	id json.RawMessage,
	threadID, commandText, feedback string,
) error {
	messages, err := encodeProviderControlTurn(id, threadID, commandText, feedback, time.Now())
	if err != nil {
		return current.writeHandoffError(ctx, id)
	}
	for _, synthetic := range messages {
		if err := current.writeDownstream(ctx, websocket.MessageText, synthetic); err != nil {
			return err
		}
	}
	return nil
}

func (current *session) handleThreadResume(ctx context.Context, message rpcMessage, payload []byte) error {
	threadID, err := requireThreadID(message)
	if err != nil || message.idKey == "" || current.coordinator == nil || message.params == nil {
		return errRoutingPolicy
	}
	seen := make(chan struct{})
	request := &desktopRequest{method: message.method, threadID: threadID, responseSeen: seen}
	if !current.trackDesktopRequest(message, request) {
		return current.writeHandoffError(ctx, message.id)
	}
	forwarded := false
	defer func() {
		if !forwarded {
			current.removeDesktopRequest(message.idKey)
		}
	}()
	current.stateMu.Lock()
	current.resumeTemplates[threadID] = cloneRawMap(message.params)
	current.stateMu.Unlock()

	release, err := current.coordinator.LockThread(ctx, threadID)
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	defer release()
	if err := current.repairRecoveryJournal(ctx, threadID); err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	targetRoute, _, err := current.selectedRoute(threadID)
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	rolloutPath, err := current.sanitizeThreadRollout(ctx, threadID, targetRoute.Provider, targetRoute)
	if errors.Is(err, rollout.ErrCompressedHistory) {
		encoded, encodeErr := encodeRPCError(message.id, handoffErrorCode, err.Error())
		if encodeErr != nil {
			return encodeErr
		}
		return current.writeDownstream(ctx, websocket.MessageText, encoded)
	}
	if errors.Is(err, rollout.ErrActiveWriter) {
		// App-server keeps an idle thread's writer lock while it is loaded. A
		// rollout that needs sanitation must therefore go through the same
		// coordinated unsubscribe/resume path as a provider handoff.
		if err := current.handoff(ctx, threadID, targetRoute); err != nil {
			return current.writeHandoffError(ctx, message.id)
		}
		// Handoff already sanitized and verified the runtime. Recovery can
		// move its rollout, so resolve by stable ID instead of scanning again
		// or replaying the path from before the handoff.
		rolloutPath = ""
		payload, err = rewriteResumePath(payload, "")
	}
	if err != nil {
		return current.writeHandoffError(ctx, message.id)
	}
	if rolloutPath != "" {
		payload, err = rewriteResumePath(payload, rolloutPath)
		if err != nil {
			return errRoutingPolicy
		}
	}
	rewritten, err := rewrite.Line(payload, targetRoute)
	if err != nil {
		return errRoutingPolicy
	}

	current.clearEffectiveProvider(threadID)
	current.setDetached(threadID, false)
	if err := current.writeUpstream(ctx, websocket.MessageText, rewritten); err != nil {
		return err
	}
	forwarded = true
	select {
	case <-seen:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-current.closed:
		return nil
	}
}

func (current *session) handoff(ctx context.Context, threadID string, targetRoute modelroute.Route) error {
	if err := current.coordinator.PrepareHandoffAll(ctx, threadID); err != nil {
		return errors.New("provider handoff capability check failed")
	}
	if err := current.coordinator.PrepareRecoveryAll(ctx, threadID); err != nil {
		return errors.New("provider recovery capability check failed")
	}
	if err := current.coordinator.MarkDirty(threadID); err != nil {
		return errors.New("provider handoff dirty marker failed")
	}
	if err := current.coordinator.UnsubscribeAll(ctx, threadID); err != nil {
		current.restoreAfterHandoffFailure(threadID)
		return errors.New("provider handoff unsubscribe failed")
	}
	if err := current.coordinator.SetDirtyStage(threadID, handoff.DirtyStageUnsubscribed); err != nil {
		current.restoreAfterHandoffFailure(threadID)
		return errors.New("provider handoff stage update failed")
	}
	resumed := false
	if _, err := current.sanitizeThreadRollout(ctx, threadID, "", targetRoute); err != nil {
		if !errors.Is(err, rollout.ErrActiveWriter) || current.recoveries == nil {
			current.restoreAfterHandoffFailure(threadID)
			return errors.New("provider handoff rollout sanitation failed")
		}
		// thread/unsubscribe removes subscriptions but Codex may keep the old
		// runtime (and its rollout writer lock) loaded. Use the journaled soft
		// unload before sanitation instead of waiting for the unload grace
		// period or resuming provider-bound history under the new provider.
		if err := current.recoverProviderMismatch(ctx, threadID, targetRoute); err != nil {
			current.restoreAfterHandoffFailure(threadID)
			return err
		}
		resumed = true
	}
	if !resumed {
		if err := current.internalResumeForHandoff(ctx, threadID, targetRoute); err != nil {
			if !errors.Is(err, errProviderMismatch) || current.recoveries == nil {
				current.restoreAfterHandoffFailure(threadID)
				return err
			}
			if err := current.coordinator.SetDirtyStage(threadID, handoff.DirtyStageResumeMismatch); err != nil {
				current.restoreAfterHandoffFailure(threadID)
				return errors.New("provider handoff stage update failed")
			}
			if err := current.recoverProviderMismatch(ctx, threadID, targetRoute); err != nil {
				current.restoreAfterHandoffFailure(threadID)
				return err
			}
		}
	}
	if err := current.coordinator.SetDirtyStage(threadID, handoff.DirtyStageResubscribing); err != nil {
		current.restoreAfterHandoffFailure(threadID)
		return errors.New("provider handoff stage update failed")
	}
	if err := current.coordinator.ResubscribeAll(ctx, threadID, targetRoute); err != nil {
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

func (current *session) internalResume(ctx context.Context, threadID string, targetRoute modelroute.Route) error {
	return current.internalResumeWithRoute(ctx, threadID, targetRoute, true)
}

func (current *session) internalResumeForHandoff(ctx context.Context, threadID string, targetRoute modelroute.Route) error {
	materialized := false
	return retryFreshRollout(ctx, current.isFresh(threadID), threadID, func(callCtx context.Context) error {
		err := current.internalResume(callCtx, threadID, targetRoute)
		if !materialized && rolloutNotReadyError(err, threadID) {
			materialized = true
			if materializeErr := current.materializeFreshRollout(callCtx, threadID, targetRoute.Provider); materializeErr != nil {
				return materializeErr
			}
		}
		return err
	})
}

func (current *session) materializeFreshRollout(ctx context.Context, threadID, targetProvider string) error {
	name := current.freshThreadName(threadID)
	if name == "" {
		name = "/provider switch " + targetProvider
	}
	_, err := current.callUpstream(ctx, "thread/name/set", map[string]json.RawMessage{
		"threadId": rawJSONString(threadID),
		"name":     rawJSONString(name),
	})
	if err != nil {
		return errors.New("materialize fresh provider rollout")
	}
	return nil
}

func (current *session) recoverProviderMismatch(ctx context.Context, threadID string, targetRoute modelroute.Route) error {
	client, err := newRecoveryClient(ctx, current.appServerSocket)
	if err != nil {
		return err
	}
	defer client.close()
	ids, _, err := client.inspectRecoverableSubtree(ctx, threadID)
	if err != nil {
		return err
	}
	if err := current.coordinator.SetDirtyStage(threadID, handoff.DirtyStageRecovering); err != nil {
		return errors.New("provider handoff stage update failed")
	}
	version := 1
	if targetRoute.Model != "" {
		version = 2
	}
	journal := recovery.Journal{
		Version: version, RootID: threadID, Provider: targetRoute.Provider, Model: targetRoute.Model, Phase: "prepared",
		Threads: append([]string(nil), ids...), Remaining: append([]string(nil), ids...),
	}
	if err := current.recoveries.Save(journal); err != nil {
		return errors.New("persist provider recovery journal")
	}
	if err := current.coordinator.BeginRecoveryAll(ctx, threadID, ids); err != nil {
		return errors.New("provider recovery peer preparation failed")
	}
	suppressionActive := true
	defer func() {
		if suppressionActive {
			repairCtx, cancel := context.WithTimeout(context.Background(), handoffRecoveryTimeout)
			defer cancel()
			_ = current.coordinator.EndRecoveryAll(repairCtx, threadID)
		}
	}()
	if err := client.archive(ctx, threadID, current.isFresh(threadID)); err != nil {
		return err
	}
	journal.Phase = "restoring"
	if err := current.recoveries.Save(journal); err != nil {
		return errors.New("update provider recovery journal")
	}
	for len(journal.Remaining) > 0 {
		if err := client.unarchive(ctx, journal.Remaining[0]); err != nil {
			return err
		}
		journal.Remaining = append([]string(nil), journal.Remaining[1:]...)
		if err := current.recoveries.Save(journal); err != nil {
			return errors.New("update provider recovery journal")
		}
	}
	if _, err := current.sanitizeThreadRollout(ctx, threadID, "", targetRoute); err != nil {
		return errors.New("provider recovery rollout sanitation failed")
	}
	if path, pathErr := current.findSanitationPath(ctx, threadID); pathErr == nil && path != "" {
		if fingerprint, fingerprintErr := rollout.Fingerprint(path); fingerprintErr == nil {
			journal.SanitizedFingerprint = fingerprint
			if err := current.recoveries.Save(journal); err != nil {
				return errors.New("record provider recovery sanitation")
			}
		}
	}
	if err := current.internalResume(ctx, threadID, targetRoute); err != nil {
		return err
	}
	if err := current.coordinator.EndRecoveryAll(ctx, threadID); err != nil {
		return errors.New("end provider recovery peer isolation")
	}
	suppressionActive = false
	if err := current.recoveries.Clear(threadID); err != nil {
		return errors.New("clear provider recovery journal")
	}
	return nil
}

func (current *session) repairRecoveryJournal(ctx context.Context, threadID string) error {
	if current.recoveries == nil {
		return nil
	}
	journal, found, err := current.recoveries.Load(threadID)
	if err != nil {
		return errors.New("read provider recovery journal")
	}
	if !found {
		return nil
	}
	if err := current.coordinator.PrepareAll(ctx, threadID); err != nil {
		return errors.New("prepare provider recovery repair")
	}
	if journal.Version == 2 {
		if err := current.coordinator.PrepareHandoffAll(ctx, threadID); err != nil {
			return errors.New("prepare provider recovery repair")
		}
	}
	client, err := newRecoveryClient(ctx, current.appServerSocket)
	if err != nil {
		return err
	}
	defer client.close()
	if err := current.coordinator.BeginRecoveryAll(ctx, threadID, journal.Threads); err != nil {
		return errors.New("prepare provider recovery repair")
	}
	suppressionActive := true
	defer func() {
		if suppressionActive {
			repairCtx, cancel := context.WithTimeout(context.Background(), handoffRecoveryTimeout)
			defer cancel()
			_ = current.coordinator.EndRecoveryAll(repairCtx, threadID)
		}
	}()

	for len(journal.Remaining) > 0 {
		id := journal.Remaining[0]
		if err := client.unarchive(ctx, id); err != nil {
			if !alreadyUnarchivedError(err, id) {
				return errors.New("repair provider recovery thread")
			}
			if thread, readErr := client.readThread(ctx, id); readErr != nil || thread.ID != id {
				return errors.New("repair provider recovery thread")
			}
		}
		journal.Remaining = append([]string(nil), journal.Remaining[1:]...)
		journal.Phase = "restoring"
		if err := current.recoveries.Save(journal); err != nil {
			return errors.New("update provider recovery repair journal")
		}
	}
	wantedRoute := modelroute.Route{Provider: journal.Provider, Model: journal.Model}
	path, err := current.findSanitationPath(ctx, threadID)
	if err != nil {
		return err
	}
	if path != "" {
		// Recheck lineage and policy; sanitation certificates skip unchanged bytes.
		repairedPath, err := current.sanitizeThreadRollout(ctx, threadID, "", wantedRoute)
		if err != nil {
			return errors.New("provider recovery repair rollout sanitation failed")
		}
		fingerprint, err := rollout.Fingerprint(repairedPath)
		if err != nil {
			return err
		}
		journal.SanitizedFingerprint = fingerprint
		journal.Phase = "restoring"
		if err := current.recoveries.Save(journal); err != nil {
			return errors.New("record provider recovery repair sanitation")
		}
	}
	route, err := client.resume(ctx, threadID, wantedRoute)
	if err != nil {
		return err
	}
	if err := client.unsubscribe(ctx, threadID); err != nil {
		return err
	}
	current.stateMu.Lock()
	current.effective[threadID] = route.Provider
	current.effectiveModel[threadID] = route.Model
	current.stateMu.Unlock()
	if err := current.coordinator.EndRecoveryAll(ctx, threadID); err != nil {
		return errors.New("end provider recovery repair isolation")
	}
	suppressionActive = false
	if err := current.recoveries.Clear(threadID); err != nil {
		return errors.New("clear provider recovery repair journal")
	}
	return nil
}

func (current *session) selectedRoute(threadID string) (modelroute.Route, bool, error) {
	route, selected, _, err := current.selectedRouteState(threadID)
	return route, selected, err
}

func (current *session) selectedRouteState(threadID string) (modelroute.Route, bool, bool, error) {
	if current.selections == nil {
		return current.routeForProvider(current.provider), current.provider != "", false, nil
	}
	selected, ok, err := current.selections.GetRoute(threadID)
	if err != nil {
		return modelroute.Route{}, false, false, errors.New("read provider selection")
	}
	if !ok {
		return current.routeForProvider(current.provider), current.provider != "", false, nil
	}
	if !providerid.Valid(selected.Provider) {
		return modelroute.Route{}, false, false, errors.New("invalid provider selection")
	}
	if selected.Model == "" {
		return current.routeForProvider(selected.Provider), true, false, nil
	}
	route, allowed := current.routes.ResolveModel(selected.Provider, selected.Model)
	if !allowed {
		return modelroute.Route{}, false, false, errors.New("invalid provider selection")
	}
	return route, true, true, nil
}

func (current *session) persistSelectedRoute(threadID string, route modelroute.Route) error {
	if current.selections == nil || !providerid.Valid(route.Provider) ||
		(route.Model != "" && !modelroute.ValidModel(route.Model)) {
		return errors.New("invalid provider selection")
	}
	return current.selections.SetRoute(threadID, selection.Value{Provider: route.Provider, Model: route.Model})
}

func (current *session) routeForProvider(provider string) modelroute.Route {
	return current.routes.Resolve(provider)
}

func routeMatches(actual, expected modelroute.Route) bool {
	return actual.Provider == expected.Provider && (expected.Model == "" || actual.Model == expected.Model)
}

func sameProvider(actual, expected modelroute.Route) bool {
	return actual.Provider != "" && actual.Provider == expected.Provider
}

func requestedModel(params map[string]json.RawMessage) (string, bool, error) {
	if params == nil {
		return "", false, nil
	}
	top, topPresent, err := rawModel(params["model"])
	if err != nil {
		return "", false, err
	}
	collaborationRaw, collaborationPresent := params["collaborationMode"]
	if !collaborationPresent || string(collaborationRaw) == "null" {
		return top, topPresent, nil
	}
	var collaboration map[string]json.RawMessage
	if json.Unmarshal(collaborationRaw, &collaboration) != nil || collaboration == nil {
		return "", false, errors.New("invalid collaboration mode")
	}
	settingsRaw, settingsPresent := collaboration["settings"]
	if !settingsPresent || string(settingsRaw) == "null" {
		return "", false, errors.New("invalid collaboration mode")
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(settingsRaw, &settings) != nil || settings == nil {
		return "", false, errors.New("invalid collaboration mode")
	}
	nested, nestedPresent, err := rawModel(settings["model"])
	if err != nil {
		return "", false, err
	}
	if nestedPresent {
		return nested, true, nil
	}
	return top, topPresent, nil
}

func rawModel(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, nil
	}
	var model string
	if json.Unmarshal(raw, &model) != nil || !modelroute.ValidModel(model) {
		return "", false, errors.New("invalid requested model")
	}
	return model, true, nil
}

func (current *session) internalResumeWithRoute(ctx context.Context, threadID string, expectedRoute modelroute.Route, reuseTemplate bool) error {
	var params map[string]json.RawMessage
	if reuseTemplate {
		current.stateMu.Lock()
		params = cloneRawMap(current.resumeTemplates[threadID])
		current.stateMu.Unlock()
	}
	if params == nil {
		params = make(map[string]json.RawMessage)
	}
	// A saved desktop resume template may contain an immutable rollout path
	// from before thread/revert. Let app-server resolve the current rollout by
	// stable thread ID instead of replaying that stale path.
	delete(params, "path")
	params["threadId"] = rawJSONString(threadID)
	// Internal resumes verify routing and restore subscriptions only. Returning
	// every turn can exceed the transport limit or hydrate gigabytes of history.
	params["excludeTurns"] = json.RawMessage("true")
	params["modelProvider"] = rawJSONString(expectedRoute.Provider)
	if expectedRoute.Model != "" {
		if err := rewrite.ApplyModel(params, expectedRoute.Model); err != nil {
			return errors.New("provider handoff resume has invalid collaboration mode")
		}
	}

	response, err := current.callUpstream(ctx, "thread/resume", params)
	if err != nil {
		return err
	}
	responseThreadID, route, ok := responseThreadRoute(response)
	if !ok || responseThreadID != threadID {
		return errors.New("provider handoff verification failed")
	}
	if !routeMatches(route, expectedRoute) {
		return errProviderMismatch
	}
	current.stateMu.Lock()
	current.effective[threadID] = route.Provider
	current.effectiveModel[threadID] = route.Model
	delete(current.detached, threadID)
	current.stateMu.Unlock()
	return nil
}

func (current *session) sanitizeThreadRollout(ctx context.Context, threadID, expectedProvider string, targets ...modelroute.Route) (string, error) {
	if current.codexHome == "" {
		return "", nil
	}
	var path string
	var runtimeRoute modelroute.Route
	var err error
	if expectedProvider == "" || expectedProvider == "openai" {
		// These paths always inspect history; only the location is needed.
		path, err = current.findSanitationPath(ctx, threadID)
	} else {
		// The same-provider skip needs live discovery, not stale DB metadata.
		path, runtimeRoute, _, err = current.findRolloutPath(ctx, threadID)
	}
	if err != nil {
		return "", err
	}
	if path == "" {
		if current.isFresh(threadID) {
			return "", nil
		}
		return "", errors.New("existing thread rollout unavailable for sanitation")
	}
	if expectedProvider != "" && expectedProvider != "openai" && runtimeRoute.Provider == expectedProvider {
		return path, nil
	}
	lockPath := filepath.Join(current.codexHome, "thread-writer-locks", threadID+".lock")
	path, err = rollout.PreparePlain(ctx, current.codexHome, threadID, path, lockPath)
	if err != nil {
		return "", err
	}
	if err := rollout.SanitizeAncestors(ctx, current.codexHome, threadID, path); err != nil {
		return "", err
	}
	var policy rollout.ContextPolicy
	if len(targets) > 0 {
		policy = rollout.ContextPolicy{Model: targets[0].Model, Compatible: current.routes.Models(targets[0].Provider)}
	}
	_, err = rollout.SanitizeFile(path, lockPath, threadID, policy)
	return path, err
}

// findRolloutPath uses thread/list because thread/read loads the thread and
// takes Codex's writer lock before sanitation can inspect the rollout.
func (current *session) findRolloutPath(ctx context.Context, threadID string) (string, modelroute.Route, string, error) {
	return current.listRolloutPath(ctx, threadID, false)
}

// findSanitationPath uses the index only for file location, never as evidence
// of the runtime provider or activity. Older/migrated indexes can have stale
// paths; preserve the normal scan-and-repair lookup as a fallback.
func (current *session) findSanitationPath(ctx context.Context, threadID string) (string, error) {
	path, _, _, err := current.listRolloutPath(ctx, threadID, true)
	if _, resolveErr := rollout.ResolvePath(current.codexHome, threadID, path); err == nil && resolveErr == nil {
		return path, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	path, _, _, err = current.findRolloutPath(ctx, threadID)
	return path, err
}

func (current *session) listRolloutPath(ctx context.Context, threadID string, stateOnly bool) (string, modelroute.Route, string, error) {
	if threadID == "" {
		return "", modelroute.Route{}, "", errors.New("invalid rollout thread")
	}
	seenCursors := make(map[string]bool)
	cursor := ""
	archived := false
	found := false
	foundPath := ""
	foundRoute := modelroute.Route{}
	foundStatus := ""
	for page := 0; page < maxRolloutListPages; page++ {
		params := map[string]json.RawMessage{
			"limit":          json.RawMessage(strconv.Itoa(rolloutListPageSize)),
			"modelProviders": json.RawMessage("[]"),
			// Internal RPCs bypass rewrite.Line; they need the same source
			// compatibility as Desktop lists to find migrated interactive tasks.
			"sourceKinds": json.RawMessage(`["cli","vscode","exec","appServer","unknown"]`),
		}
		if archived {
			params["archived"] = json.RawMessage("true")
		}
		if cursor != "" {
			params["cursor"] = rawJSONString(cursor)
		}
		if stateOnly {
			params["useStateDbOnly"] = json.RawMessage("true")
		}
		response, err := current.callUpstream(ctx, "thread/list", params)
		if err != nil {
			return "", modelroute.Route{}, "", errors.New("list rollout threads")
		}
		var result struct {
			Data []struct {
				ID            string  `json:"id"`
				Path          *string `json:"path"`
				ModelProvider string  `json:"modelProvider"`
				Model         string  `json:"model"`
				Status        struct {
					Type string `json:"type"`
				} `json:"status"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		encoded, marshalErr := json.Marshal(response.result)
		if marshalErr != nil || json.Unmarshal(encoded, &result) != nil || result.Data == nil {
			return "", modelroute.Route{}, "", errors.New("invalid rollout thread list")
		}
		for _, thread := range result.Data {
			if thread.ID == "" {
				return "", modelroute.Route{}, "", errors.New("invalid rollout thread list")
			}
			if thread.ID == threadID {
				if found {
					return "", modelroute.Route{}, "", errors.New("ambiguous rollout thread list")
				}
				found = true
				foundRoute = modelroute.Route{Provider: thread.ModelProvider, Model: thread.Model}
				foundStatus = thread.Status.Type
				if thread.Path != nil {
					foundPath = *thread.Path
				}
			}
		}
		if result.NextCursor == nil || *result.NextCursor == "" {
			if found {
				return foundPath, foundRoute, foundStatus, nil
			}
			if !archived {
				archived = true
				cursor = ""
				seenCursors = make(map[string]bool)
				continue
			}
			return "", modelroute.Route{}, "", nil
		}
		if seenCursors[*result.NextCursor] {
			return "", modelroute.Route{}, "", errors.New("invalid rollout thread cursor")
		}
		seenCursors[*result.NextCursor] = true
		cursor = *result.NextCursor
	}
	return "", modelroute.Route{}, "", errors.New("rollout thread list exceeds limit")
}

func rewriteResumePath(payload []byte, path string) ([]byte, error) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal(payload, &message); err != nil || message == nil {
		return nil, errors.New("invalid resume request")
	}
	var params map[string]json.RawMessage
	if raw, ok := message["params"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil || params == nil {
			return nil, errors.New("resume params must be an object")
		}
	} else {
		params = make(map[string]json.RawMessage)
	}
	if path == "" {
		delete(params, "path")
	} else {
		params["path"] = rawJSONString(path)
	}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return nil, errors.New("encode resume params")
	}
	message["params"] = encodedParams
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, errors.New("encode resume request")
	}
	return encoded, nil
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
			if response.errorMessage != "" {
				return rpcMessage{}, &appServerRPCError{code: response.errorCode, message: response.errorMessage}
			}
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
func (current *session) Resubscribe(ctx context.Context, threadID string, route modelroute.Route) (handoff.PeerStatus, error) {
	if !current.isDetached(threadID) {
		return handoff.StatusNotSubscribed, nil
	}
	if !providerid.Valid(route.Provider) || (route.Model != "" && !modelroute.ValidModel(route.Model)) {
		return "", errors.New("invalid provider handoff resubscribe")
	}
	if err := current.internalResumeWithRoute(ctx, threadID, route, false); err != nil {
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
		"threadId":     rawJSONString(threadID),
		"excludeTurns": json.RawMessage("true"),
	})
	if err != nil {
		return "", errors.New("restore app-server thread")
	}
	responseThreadID, route, ok := responseThreadRoute(response)
	if !ok || responseThreadID != threadID {
		return "", errors.New("verify restored app-server thread")
	}
	current.stateMu.Lock()
	current.effective[threadID] = route.Provider
	current.effectiveModel[threadID] = route.Model
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

func (current *session) writeProviderCommandError(ctx context.Context, id json.RawMessage) error {
	payload, err := encodeRPCError(id, providerCommandErrorCode, invalidProviderCommandMessage)
	if err != nil {
		return err
	}
	return current.writeDownstream(ctx, websocket.MessageText, payload)
}

func (current *session) trackDesktopRequest(message rpcMessage, request *desktopRequest) bool {
	if message.idKey == "" {
		return false
	}
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	if _, exists := current.desktop[message.idKey]; exists {
		return false
	}
	if _, exists := current.internal[message.idKey]; exists {
		return false
	}
	current.desktop[message.idKey] = request
	return true
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
	if current.suppressRecoveryNotification(message) {
		return nil
	}
	var forkWarning []byte

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
			request.responseOK = !message.hasError
			if request.method == "turn/start" {
				request.responseOK = responseTurnAccepted(message)
			}
			if threadID, route, ok := responseThreadRoute(message); ok &&
				(request.method == "thread/start" || request.method == "thread/resume" || request.method == "thread/fork") {
				current.effective[threadID] = route.Provider
				current.effectiveModel[threadID] = route.Model
				delete(current.detached, threadID)
				if request.method == "thread/start" {
					current.fresh[threadID] = responseThreadName(message)
				}
			}
			if request.method == "turn/start" {
				if !request.responseOK {
					delete(current.active, request.threadID)
				} else {
					delete(current.fresh, request.threadID)
					// turn/start can update the model but cannot switch providers.
					// Its acknowledgement must not turn a desired provider into
					// an observed runtime provider.
					if providerid.Valid(request.targetRoute.Provider) &&
						current.effective[request.threadID] == request.targetRoute.Provider {
						current.effectiveModel[request.threadID] = request.targetRoute.Model
					}
				}
			}
			if request.method == "thread/unsubscribe" && !message.hasError {
				delete(current.effective, request.threadID)
				delete(current.effectiveModel, request.threadID)
				delete(current.detached, request.threadID)
			}
		}
		current.stateMu.Unlock()
		if request != nil && request.method == "thread/fork" && !message.hasError {
			payload, forkWarning = current.completeForkPreview(ctx, payload)
		}
		if request != nil && request.releaseThread != nil {
			request.releaseThread()
		}
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

	if err := current.writeDownstream(ctx, websocket.MessageText, payload); err != nil {
		return err
	}
	if len(forkWarning) != 0 {
		return current.writeDownstream(ctx, websocket.MessageText, forkWarning)
	}
	return nil
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

func (current *session) effectiveRoute(threadID string) modelroute.Route {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	return modelroute.Route{Provider: current.effective[threadID], Model: current.effectiveModel[threadID]}
}

func (current *session) effectiveRouteForTurn(ctx context.Context, threadID string) (modelroute.Route, error) {
	route := current.effectiveRoute(threadID)
	if route.Provider != "" {
		return route, nil
	}

	_, runtime, _, err := current.findRolloutPath(ctx, threadID)
	if err != nil {
		return modelroute.Route{}, err
	}
	if runtime.Provider == "" {
		return route, nil
	}
	if !providerid.Valid(runtime.Provider) || (runtime.Model != "" && !modelroute.ValidModel(runtime.Model)) {
		return modelroute.Route{}, errors.New("invalid runtime route")
	}

	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	if current.effective[threadID] == "" {
		current.effective[threadID] = runtime.Provider
		current.effectiveModel[threadID] = runtime.Model
	}
	return modelroute.Route{
		Provider: current.effective[threadID],
		Model:    current.effectiveModel[threadID],
	}, nil
}

func (current *session) clearEffectiveProvider(threadID string) {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	delete(current.effective, threadID)
	delete(current.effectiveModel, threadID)
}

func (current *session) clearThreadRoutingState(threadID string) {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	delete(current.effective, threadID)
	delete(current.effectiveModel, threadID)
	delete(current.fresh, threadID)
	delete(current.detached, threadID)
}

func (current *session) isFresh(threadID string) bool {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	_, ok := current.fresh[threadID]
	return ok
}

func (current *session) freshThreadName(threadID string) string {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	return current.fresh[threadID]
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

// ReconcileIdle clears a stale active notification cache after the
// coordinating caller has verified authoritative quiescence under the shared
// thread lock.
func (current *session) ReconcileIdle(threadID string) handoff.PeerStatus {
	current.setActive(threadID, false)
	return handoff.StatusReady
}

// BeginRecovery installs exact notification suppression for one transaction.
func (current *session) BeginRecovery(threadID string, ids []string) handoff.PeerStatus {
	if threadID == "" || len(ids) == 0 || len(ids) > maxRecoveryThreads || current.isActive(threadID) {
		return handoff.StatusBusy
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || set[id] {
			return handoff.StatusBusy
		}
		set[id] = true
	}
	if !set[threadID] {
		return handoff.StatusBusy
	}
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	current.recovery[threadID] = set
	return handoff.StatusRecoveryReady
}

// EndRecovery removes exact notification suppression for one transaction.
func (current *session) EndRecovery(threadID string) handoff.PeerStatus {
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	delete(current.recovery, threadID)
	return handoff.StatusRecoveryEnded
}

func (current *session) suppressRecoveryNotification(message rpcMessage) bool {
	if message.kind != rpcNotification || message.threadID == "" {
		return false
	}
	switch message.method {
	case "thread/archived", "thread/unarchived", "thread/closed", "thread/status/changed":
	default:
		return false
	}
	current.stateMu.Lock()
	defer current.stateMu.Unlock()
	for _, ids := range current.recovery {
		if ids[message.threadID] {
			return true
		}
	}
	return false
}

func (current *session) closeState() {
	current.closeOnce.Do(func() {
		current.stateMu.Lock()
		requests := current.desktop
		current.desktop = make(map[string]*desktopRequest)
		current.internal = make(map[string]chan rpcMessage)
		current.stateMu.Unlock()
		for _, request := range requests {
			if request.releaseThread != nil {
				request.releaseThread()
			}
			if request.responseSeen != nil {
				close(request.responseSeen)
			}
		}
		close(current.closed)
	})
}
