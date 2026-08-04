package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/recovery"
)

func TestRunSwitchesProviderOnNextTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	openai, openaiDone := dialProviderProxy(t, ctx, server.socket, "openai")
	sub2api, sub2apiDone := dialProviderProxy(t, ctx, server.socket, "sub2api")
	defer openai.CloseNow()
	defer sub2api.CloseNow()

	initializeTestClient(t, ctx, openai)
	initializeTestClient(t, ctx, sub2api)
	if provider := resumeTestThread(t, ctx, openai, 2); provider != "openai" {
		t.Fatalf("openai resume provider = %q", provider)
	}
	if provider := resumeTestThread(t, ctx, sub2api, 2); provider != "openai" {
		t.Fatalf("sub2api initial resume provider = %q, want loaded openai", provider)
	}

	subVisible := sendTestTurnAndCollect(t, ctx, sub2api, 3)
	if record := <-server.turns; record.threadID != "thr-shared" || record.provider != "sub2api" {
		t.Fatalf("sub2api turn = %#v", record)
	}
	assertNoInternalMessages(t, subVisible)
	if subscribers := server.subscriberCount(); subscribers != 2 {
		t.Fatalf("subscriber count after switch = %d, want 2", subscribers)
	}
	readUntilMethod(t, ctx, openai, "turn/started")
	readUntilMethod(t, ctx, openai, "turn/completed")

	openVisible := sendTestTurnAndCollect(t, ctx, openai, 3)
	if record := <-server.turns; record.threadID != "thr-shared" || record.provider != "openai" {
		t.Fatalf("openai turn = %#v", record)
	}
	assertNoInternalMessages(t, openVisible)

	cancel()
	_ = openai.CloseNow()
	_ = sub2api.CloseNow()
	waitProxyDone(t, openaiDone)
	waitProxyDone(t, sub2apiDone)
}

func TestProviderDesktopMarkdownSkillCommandSwitchesWithoutModelTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	connection, done := dialProviderProxy(t, ctx, server.socket, "openai")
	defer connection.CloseNow()

	initializeTestClient(t, ctx, connection)
	if provider := resumeTestThread(t, ctx, connection, 2); provider != "openai" {
		t.Fatalf("initial provider = %q", provider)
	}
	sendRPC(t, ctx, connection, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input": []any{
			map[string]any{
				"type":          "text",
				"text":          "[$provider](/home/user/.agents/skills/provider/SKILL.md) status",
				"text_elements": []any{},
			},
		},
	})
	statusResponseSeen, statusMethods, statusFeedback := readProviderControlLifecycle(t, ctx, connection, "3")
	if !statusResponseSeen || statusFeedback != "Runtime provider: openai (verified).\nSelected provider: openai." {
		t.Fatalf("status lifecycle response=%v methods=%v feedback=%q", statusResponseSeen, statusMethods, statusFeedback)
	}
	if calls := server.turnStartCallCount(); calls != 0 {
		t.Fatalf("provider status app-server turn/start calls = %d, want 0", calls)
	}

	sendRPC(t, ctx, connection, 4, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input": []any{
			map[string]any{
				"type":          "text",
				"text":          "[$provider](/home/user/.agents/skills/provider/SKILL.md) switch sub2api",
				"text_elements": []any{},
			},
		},
	})
	responseSeen, methods, feedback := readProviderControlLifecycle(t, ctx, connection, "4")
	if !responseSeen || feedback != "Provider switched to sub2api." {
		t.Fatalf("switch lifecycle response=%v methods=%v feedback=%q", responseSeen, methods, feedback)
	}
	if calls := server.turnStartCallCount(); calls != 0 {
		t.Fatalf("provider command app-server turn/start calls = %d, want 0", calls)
	}
	select {
	case unexpected := <-server.turns:
		t.Fatalf("provider command reached model turn: %#v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}

	visible := sendTestTurnAndCollect(t, ctx, connection, 5)
	assertNoInternalMessages(t, visible)
	if record := <-server.turns; record.threadID != "thr-shared" || record.provider != "sub2api" {
		t.Fatalf("ordinary turn after command = %#v", record)
	}

	cancel()
	_ = connection.CloseNow()
	waitProxyDone(t, done)
}

func TestProviderCommandWaitsForFreshThreadRollout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	server.mu.Lock()
	server.freshNeedsMaterialize = true
	server.archiveNotReadyFailures = 2
	server.stickyIdle = true
	server.mu.Unlock()
	connection, done := dialProviderProxy(t, ctx, server.socket, "openai")
	defer connection.CloseNow()

	initializeTestClient(t, ctx, connection)
	sendRPC(t, ctx, connection, 2, "thread/start", map[string]any{"cwd": "/tmp"})
	if provider := responseProvider(t, readResponse(t, ctx, connection, "2").raw); provider != "openai" {
		t.Fatalf("fresh thread provider = %q, want openai", provider)
	}

	sendRPC(t, ctx, connection, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input": []any{map[string]any{
			"type": "text", "text": "/provider switch sub2api",
		}},
	})
	response := readResponse(t, ctx, connection, "3")
	if response.errorCode != 0 {
		t.Fatalf("fresh provider switch response = %#v", response)
	}
	feedback := ""
	for range 7 {
		message := readVisibleRPC(t, ctx, connection)
		if message.method == "item/agentMessage/delta" {
			feedback = message.delta
		}
	}
	if feedback != "Provider switched to sub2api." {
		t.Fatalf("fresh provider switch feedback = %q", feedback)
	}
	server.mu.Lock()
	provider := server.provider
	archiveCalls := server.archiveCalls
	materializeCalls := server.materializeCalls
	nameSetValue := server.nameSetValue
	server.mu.Unlock()
	if provider != "sub2api" || archiveCalls != 3 || materializeCalls != 1 ||
		nameSetValue != "/provider switch sub2api" {
		t.Fatalf("fresh provider=%q archive calls=%d materialize calls=%d name=%q",
			provider, archiveCalls, materializeCalls, nameSetValue)
	}
	if calls := server.turnStartCallCount(); calls != 0 {
		t.Fatalf("fresh provider switch model turns=%d, want 0", calls)
	}

	cancel()
	_ = connection.CloseNow()
	waitProxyDone(t, done)
}

func TestProviderCommandDoesNotWaitForExistingThreadRollout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	server.mu.Lock()
	server.archiveNotReadyFailures = 2
	server.stickyIdle = true
	server.mu.Unlock()
	connection, done := dialProviderProxy(t, ctx, server.socket, "openai")
	defer connection.CloseNow()

	initializeTestClient(t, ctx, connection)
	if provider := resumeTestThread(t, ctx, connection, 2); provider != "openai" {
		t.Fatalf("existing thread provider = %q, want openai", provider)
	}
	sendRPC(t, ctx, connection, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input": []any{map[string]any{
			"type": "text", "text": "/provider switch sub2api",
		}},
	})
	response := readResponse(t, ctx, connection, "3")
	if response.errorCode != handoffErrorCode {
		t.Fatalf("existing provider switch response = %#v", response)
	}
	server.mu.Lock()
	archiveCalls := server.archiveCalls
	server.mu.Unlock()
	if archiveCalls != 1 {
		t.Fatalf("existing thread archive calls=%d, want 1", archiveCalls)
	}
	if calls := server.turnStartCallCount(); calls != 0 {
		t.Fatalf("existing provider switch model turns=%d, want 0", calls)
	}

	cancel()
	_ = connection.CloseNow()
	waitProxyDone(t, done)
}

func TestRunRecoversSystemErrorProviderWithoutModelTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	server.mu.Lock()
	server.systemError = true
	server.history = []string{"failed-turn-fixture"}
	server.descendants = []string{"thr-child"}
	server.mu.Unlock()
	connection, done := dialProviderProxy(t, ctx, server.socket, "openai")
	defer connection.CloseNow()

	initializeTestClient(t, ctx, connection)
	if provider := resumeTestThread(t, ctx, connection, 2); provider != "openai" {
		t.Fatalf("initial provider = %q", provider)
	}
	sendRPC(t, ctx, connection, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input": []any{map[string]any{
			"type": "text", "text": "[$provider](/home/user/.agents/skills/provider/SKILL.md) switch sub2api",
		}},
	})
	response := readResponse(t, ctx, connection, "3")
	if response.errorCode != 0 {
		t.Fatalf("recovery response = %#v", response)
	}
	feedback := ""
	for range 7 {
		message := readVisibleRPC(t, ctx, connection)
		if message.method == "item/agentMessage/delta" {
			feedback = message.delta
		}
	}
	if feedback != "Provider switched to sub2api." {
		t.Fatalf("recovery feedback=%q", feedback)
	}

	server.mu.Lock()
	provider := server.provider
	archiveCalls := server.archiveCalls
	unarchived := append([]string(nil), server.unarchived...)
	history := append([]string(nil), server.history...)
	server.mu.Unlock()
	if provider != "sub2api" || archiveCalls != 1 {
		t.Fatalf("recovered provider=%q archive calls=%d", provider, archiveCalls)
	}
	if fmt.Sprint(unarchived) != fmt.Sprint([]string{"thr-child", "thr-shared"}) {
		t.Fatalf("unarchived = %v", unarchived)
	}
	if fmt.Sprint(history) != fmt.Sprint([]string{"failed-turn-fixture"}) {
		t.Fatalf("history = %v", history)
	}
	if calls := server.turnStartCallCount(); calls != 0 {
		t.Fatalf("recovery turn/start calls = %d, want 0", calls)
	}

	cancel()
	_ = connection.CloseNow()
	waitProxyDone(t, done)
}

func TestRunRepairsPersistedRecoveryBeforeProviderSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	stateDir := t.TempDir()
	store, err := recovery.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	journal := recovery.Journal{
		Version: 1, RootID: "thr-shared", Provider: "sub2api", Phase: "restoring",
		Threads: []string{"thr-child", "thr-shared"}, Remaining: []string{"thr-child", "thr-shared"},
	}
	if err := store.Save(journal); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.archived["thr-child"] = true
	server.archived["thr-shared"] = true
	server.mu.Unlock()
	connection, done := dialProviderProxyOptions(t, ctx, config.Config{
		Provider: "openai", Socket: server.socket, StateDir: stateDir,
	})
	defer connection.CloseNow()
	initializeTestClient(t, ctx, connection)
	if provider := resumeTestThread(t, ctx, connection, 2); provider != "openai" {
		t.Fatalf("provider after resume repair = %q", provider)
	}
	if _, ok, err := store.Load("thr-shared"); err != nil || ok {
		t.Fatalf("journal after resume repair = ok %v, err %v", ok, err)
	}

	sendRPC(t, ctx, connection, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input": []any{map[string]any{
			"type": "text", "text": "[$provider](/home/user/.agents/skills/provider/SKILL.md) switch sub2api",
		}},
	})
	response := readResponse(t, ctx, connection, "3")
	if response.errorCode != 0 {
		t.Fatalf("repair response = %#v", response)
	}
	for range 7 {
		_ = readVisibleRPC(t, ctx, connection)
	}
	server.mu.Lock()
	unarchived := append([]string(nil), server.unarchived...)
	server.mu.Unlock()
	if fmt.Sprint(unarchived) != fmt.Sprint([]string{"thr-child", "thr-shared"}) {
		t.Fatalf("repair unarchived = %v", unarchived)
	}
	if calls := server.turnStartCallCount(); calls != 0 {
		t.Fatalf("repair turn/start calls = %d", calls)
	}

	cancel()
	_ = connection.CloseNow()
	waitProxyDone(t, done)
}

func TestRunRetainsJournalWhenRecoveryRepairIsUncertain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	stateDir := t.TempDir()
	connection, done := dialProviderProxyOptions(t, ctx, config.Config{
		Provider: "openai", Socket: server.socket, StateDir: stateDir,
	})
	defer connection.CloseNow()
	initializeTestClient(t, ctx, connection)
	resumeTestThread(t, ctx, connection, 2)

	store, err := recovery.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	journal := recovery.Journal{
		Version: 1, RootID: "thr-shared", Provider: "sub2api", Phase: "restoring",
		Threads: []string{"thr-child", "thr-shared"}, Remaining: []string{"thr-child", "thr-shared"},
	}
	if err := store.Save(journal); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.archived["thr-child"] = true
	server.archived["thr-shared"] = true
	server.failUnarchive = "thr-child"
	server.mu.Unlock()

	sendRPC(t, ctx, connection, 3, "turn/start", map[string]any{
		"threadId": "thr-shared", "input": []any{},
	})
	response := readResponse(t, ctx, connection, "3")
	if response.errorCode != handoffErrorCode {
		t.Fatalf("uncertain repair response = %#v", response)
	}
	if _, ok, err := store.Load("thr-shared"); err != nil || !ok {
		t.Fatalf("journal after uncertain repair = ok %v, err %v", ok, err)
	}
	if calls := server.turnStartCallCount(); calls != 0 {
		t.Fatalf("uncertain repair turn/start calls = %d", calls)
	}

	cancel()
	_ = connection.CloseNow()
	waitProxyDone(t, done)
}

func TestRunRepairsJournalWrittenBeforeArchive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	stateDir := t.TempDir()
	store, err := recovery.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	journal := recovery.Journal{
		Version: 1, RootID: "thr-shared", Provider: "sub2api", Phase: "prepared",
		Threads: []string{"thr-child", "thr-shared"}, Remaining: []string{"thr-child", "thr-shared"},
	}
	if err := store.Save(journal); err != nil {
		t.Fatal(err)
	}
	connection, done := dialProviderProxyOptions(t, ctx, config.Config{
		Provider: "openai", Socket: server.socket, StateDir: stateDir,
	})
	defer connection.CloseNow()
	initializeTestClient(t, ctx, connection)
	if provider := resumeTestThread(t, ctx, connection, 2); provider != "openai" {
		t.Fatalf("provider after pre-archive repair = %q", provider)
	}
	if _, ok, err := store.Load("thr-shared"); err != nil || ok {
		t.Fatalf("pre-archive journal after repair = ok %v, err %v", ok, err)
	}
	server.mu.Lock()
	unarchived := append([]string(nil), server.unarchived...)
	server.mu.Unlock()
	if len(unarchived) != 0 {
		t.Fatalf("pre-archive repair moved threads = %v", unarchived)
	}

	cancel()
	_ = connection.CloseNow()
	waitProxyDone(t, done)
}

func TestRunRejectsConcurrentSameProviderTurnBeforePeerNotification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, false)
	server.startedGate = make(chan struct{})
	first, firstDone := dialProviderProxy(t, ctx, server.socket, "openai")
	second, secondDone := dialProviderProxy(t, ctx, server.socket, "openai")
	defer first.CloseNow()
	defer second.CloseNow()

	initializeTestClient(t, ctx, first)
	initializeTestClient(t, ctx, second)
	resumeTestThread(t, ctx, first, 2)
	resumeTestThread(t, ctx, second, 2)

	sendRPC(t, ctx, first, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	if record := <-server.turns; record.provider != "openai" {
		t.Fatalf("first turn = %#v", record)
	}
	sendRPC(t, ctx, second, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	response := readResponse(t, ctx, second, `3`)
	if response.errorCode != handoffErrorCode || response.errorMessage != handoffUnavailableMessage {
		t.Fatalf("concurrent turn response = %#v", response)
	}
	if calls := server.turnStartCallCount(); calls != 1 {
		t.Fatalf("app-server turn/start calls = %d, want 1", calls)
	}

	cancel()
	_ = first.CloseNow()
	_ = second.CloseNow()
	waitProxyDone(t, firstDone)
	waitProxyDone(t, secondDone)
}

func TestRunRejectsHandoffWhilePeerTurnIsActive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, false)
	openai, openaiDone := dialProviderProxy(t, ctx, server.socket, "openai")
	sub2api, sub2apiDone := dialProviderProxy(t, ctx, server.socket, "sub2api")
	defer openai.CloseNow()
	defer sub2api.CloseNow()

	initializeTestClient(t, ctx, openai)
	initializeTestClient(t, ctx, sub2api)
	resumeTestThread(t, ctx, openai, 2)
	resumeTestThread(t, ctx, sub2api, 2)

	sendRPC(t, ctx, openai, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	readUntilMethod(t, ctx, openai, "turn/started")
	if record := <-server.turns; record.provider != "openai" {
		t.Fatalf("active turn = %#v", record)
	}

	sendRPC(t, ctx, sub2api, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	response := readResponse(t, ctx, sub2api, `3`)
	if response.errorCode != handoffErrorCode || response.errorMessage != handoffUnavailableMessage {
		t.Fatalf("handoff response = %#v", response)
	}
	select {
	case unexpected := <-server.turns:
		t.Fatalf("blocked turn reached app-server: %#v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}
	if subscribers := server.subscriberCount(); subscribers != 2 {
		t.Fatalf("subscriber count = %d, want 2 after prepare rejection", subscribers)
	}

	cancel()
	_ = openai.CloseNow()
	_ = sub2api.CloseNow()
	waitProxyDone(t, openaiDone)
	waitProxyDone(t, sub2apiDone)
}

func TestRunRejectsUnknownProviderMismatchWithoutArchive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	server.mu.Lock()
	server.stickyIdle = true
	server.readStatus = "mystery"
	server.mu.Unlock()
	sub2api, sub2apiDone := dialProviderProxy(t, ctx, server.socket, "sub2api")
	defer sub2api.CloseNow()
	initializeTestClient(t, ctx, sub2api)
	if provider := resumeTestThread(t, ctx, sub2api, 2); provider != "openai" {
		t.Fatalf("sub2api initial resume provider = %q", provider)
	}
	sendRPC(t, ctx, sub2api, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	response := readResponse(t, ctx, sub2api, `3`)
	if response.errorCode != handoffErrorCode {
		t.Fatalf("unknown mismatch response = %#v", response)
	}
	server.mu.Lock()
	archiveCalls := server.archiveCalls
	server.mu.Unlock()
	if archiveCalls != 0 {
		t.Fatalf("unknown mismatch archive calls=%d, want 0", archiveCalls)
	}
	if calls := server.turnStartCallCount(); calls != 0 {
		t.Fatalf("unknown mismatch turn/start calls=%d, want 0", calls)
	}
	if subscribers := server.subscriberCount(); subscribers != 1 {
		t.Fatalf("unknown mismatch subscribers=%d, want 1 after restoration", subscribers)
	}

	cancel()
	_ = sub2api.CloseNow()
	waitProxyDone(t, sub2apiDone)
}

func TestRunRecoversIdleProviderMismatchWithoutModelTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newHandoffAppServer(t, ctx, true)
	server.mu.Lock()
	server.stickyIdle = true
	server.history = []string{"idle-history-fixture"}
	server.descendants = []string{"thr-child"}
	server.mu.Unlock()

	stateDir := t.TempDir()
	openai, openaiDone := dialProviderProxyOptions(t, ctx, config.Config{
		Provider: "openai", Socket: server.socket, StateDir: stateDir,
	})
	sub2api, sub2apiDone := dialProviderProxyOptions(t, ctx, config.Config{
		Provider: "sub2api", Socket: server.socket, StateDir: stateDir,
	})
	defer openai.CloseNow()
	defer sub2api.CloseNow()
	initializeTestClient(t, ctx, openai)
	initializeTestClient(t, ctx, sub2api)
	if provider := resumeTestThread(t, ctx, openai, 2); provider != "openai" {
		t.Fatalf("openai resume provider = %q", provider)
	}
	if provider := resumeTestThread(t, ctx, sub2api, 2); provider != "openai" {
		t.Fatalf("sub2api initial resume provider = %q", provider)
	}

	sendRPC(t, ctx, sub2api, 3, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input": []any{map[string]any{
			"type": "text", "text": "[$provider](/home/user/.agents/skills/provider/SKILL.md) switch sub2api",
		}},
	})
	responseSeen, methods, feedback := readProviderControlLifecycle(t, ctx, sub2api, "3")
	if !responseSeen || feedback != "Provider switched to sub2api." {
		t.Fatalf("idle recovery response=%v methods=%v feedback=%q", responseSeen, methods, feedback)
	}
	server.mu.Lock()
	provider := server.provider
	archiveCalls := server.archiveCalls
	unarchived := append([]string(nil), server.unarchived...)
	history := append([]string(nil), server.history...)
	server.mu.Unlock()
	if provider != "sub2api" || archiveCalls != 1 {
		t.Fatalf("idle recovery provider=%q archive calls=%d", provider, archiveCalls)
	}
	if fmt.Sprint(unarchived) != fmt.Sprint([]string{"thr-child", "thr-shared"}) {
		t.Fatalf("idle recovery unarchived=%v", unarchived)
	}
	if fmt.Sprint(history) != fmt.Sprint([]string{"idle-history-fixture"}) {
		t.Fatalf("idle recovery history=%v", history)
	}
	if calls := server.turnStartCallCount(); calls != 0 {
		t.Fatalf("idle recovery turn/start calls=%d, want 0", calls)
	}
	if subscribers := server.subscriberCount(); subscribers != 2 {
		t.Fatalf("idle recovery subscribers=%d, want 2", subscribers)
	}
	store, err := recovery.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Load("thr-shared"); err != nil || found {
		t.Fatalf("idle recovery journal found=%v err=%v", found, err)
	}
	assertNoVisibleMessageWithin(t, ctx, openai, 100*time.Millisecond)
	assertNoVisibleMessageWithin(t, ctx, sub2api, 100*time.Millisecond)

	cancel()
	_ = openai.CloseNow()
	_ = sub2api.CloseNow()
	waitProxyDone(t, openaiDone)
	waitProxyDone(t, sub2apiDone)
}

type handoffAppServer struct {
	socket       string
	ctx          context.Context
	autoComplete bool

	mu                      sync.Mutex
	writeMu                 sync.Mutex
	nextClient              int
	provider                string
	active                  bool
	subscribers             map[int]*websocket.Conn
	clients                 map[int]*websocket.Conn
	startedGate             chan struct{}
	turnStarts              int
	turns                   chan handoffTurnRecord
	systemError             bool
	descendants             []string
	history                 []string
	archiveCalls            int
	archiveNotReadyFailures int
	freshNeedsMaterialize   bool
	rolloutReady            bool
	materializeCalls        int
	nameSetValue            string
	unarchived              []string
	archived                map[string]bool
	failUnarchive           string
	stickyIdle              bool
	softReloaded            bool
	readStatus              string
}

type handoffTurnRecord struct {
	threadID string
	provider string
}

func newHandoffAppServer(t *testing.T, ctx context.Context, autoComplete bool) *handoffAppServer {
	t.Helper()
	server := &handoffAppServer{
		ctx:          ctx,
		autoComplete: autoComplete,
		provider:     "openai",
		subscribers:  make(map[int]*websocket.Conn),
		clients:      make(map[int]*websocket.Conn),
		turns:        make(chan handoffTurnRecord, 8),
		archived:     make(map[string]bool),
	}
	server.socket = startUnixHTTPServer(t, http.HandlerFunc(server.handleUpgrade))
	return server
}

func (server *handoffAppServer) handleUpgrade(writer http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	server.mu.Lock()
	server.nextClient++
	clientID := server.nextClient
	server.clients[clientID] = connection
	server.mu.Unlock()
	defer func() {
		server.mu.Lock()
		delete(server.subscribers, clientID)
		delete(server.clients, clientID)
		server.mu.Unlock()
	}()

	for {
		messageType, payload, err := connection.Read(server.ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		message, err := parseRPCMessage(payload)
		if err != nil || message.kind != rpcRequest {
			continue
		}
		switch message.method {
		case "initialize":
			server.writeResult(connection, message.id, map[string]any{"userAgent": "handoff-test"})
		case "thread/start":
			server.handleStart(connection, clientID, message)
		case "thread/name/set":
			server.handleNameSet(connection, message)
		case "thread/resume":
			server.handleResume(connection, clientID, message)
		case "thread/unsubscribe":
			server.handleUnsubscribe(connection, clientID, message)
		case "thread/read":
			server.handleRead(connection, message)
		case "thread/list":
			server.handleList(connection, message)
		case "thread/archive":
			server.handleArchive(connection, message)
		case "thread/unarchive":
			server.handleUnarchive(connection, message)
		case "turn/start":
			server.handleTurnStart(connection, message)
		default:
			server.writeResult(connection, message.id, map[string]any{})
		}
	}
}

func (server *handoffAppServer) handleStart(connection *websocket.Conn, clientID int, message rpcMessage) {
	requestedProvider := ""
	_ = json.Unmarshal(message.params["modelProvider"], &requestedProvider)
	server.mu.Lock()
	if requestedProvider != "" {
		server.provider = requestedProvider
	}
	server.subscribers[clientID] = connection
	provider := server.provider
	server.mu.Unlock()
	server.writeResult(connection, message.id, map[string]any{
		"thread": map[string]any{
			"id": "thr-shared", "turns": []any{}, "status": map[string]any{"type": "idle"},
		},
		"modelProvider": provider,
	})
}

func (server *handoffAppServer) handleResume(connection *websocket.Conn, clientID int, message rpcMessage) {
	requestedProvider := ""
	_ = json.Unmarshal(message.params["modelProvider"], &requestedProvider)
	server.mu.Lock()
	if requestedProvider != "" && requestedProvider != server.provider &&
		server.freshNeedsMaterialize && !server.rolloutReady {
		server.mu.Unlock()
		server.writeError(connection, message.id, -32600, "no rollout found for thread id thr-shared")
		return
	}
	if server.archived["thr-shared"] {
		server.mu.Unlock()
		server.writeError(connection, message.id, -32001, "thread archived")
		return
	}
	if requestedProvider != "" && requestedProvider != server.provider && len(server.subscribers) == 0 &&
		!server.active && !server.systemError && (!server.stickyIdle || server.softReloaded) {
		server.provider = requestedProvider
	}
	server.subscribers[clientID] = connection
	provider := server.provider
	status := "idle"
	if server.systemError {
		status = "systemError"
	}
	history := append([]string(nil), server.history...)
	server.mu.Unlock()
	server.writeResult(connection, message.id, map[string]any{
		"thread":        map[string]any{"id": "thr-shared", "turns": history, "status": map[string]any{"type": status}},
		"modelProvider": provider,
	})
}

func (server *handoffAppServer) handleNameSet(connection *websocket.Conn, message rpcMessage) {
	threadID, _ := requireThreadID(message)
	name := ""
	_ = json.Unmarshal(message.params["name"], &name)
	server.mu.Lock()
	if threadID == "thr-shared" && name != "" && server.freshNeedsMaterialize && !server.rolloutReady {
		server.rolloutReady = true
		server.materializeCalls++
		server.nameSetValue = name
	}
	server.mu.Unlock()
	server.writeResult(connection, message.id, map[string]any{})
}

func (server *handoffAppServer) handleRead(connection *websocket.Conn, message rpcMessage) {
	threadID, _ := requireThreadID(message)
	server.mu.Lock()
	status := "idle"
	if server.systemError && threadID == "thr-shared" {
		status = "systemError"
	}
	if server.readStatus != "" && threadID == "thr-shared" {
		status = server.readStatus
	}
	history := append([]string(nil), server.history...)
	server.mu.Unlock()
	server.writeResult(connection, message.id, map[string]any{"thread": map[string]any{
		"id": threadID, "status": map[string]any{"type": status}, "turns": history,
	}})
}

func (server *handoffAppServer) handleList(connection *websocket.Conn, message rpcMessage) {
	server.mu.Lock()
	descendants := append([]string(nil), server.descendants...)
	server.mu.Unlock()
	data := make([]map[string]any, 0, len(descendants))
	for _, id := range descendants {
		data = append(data, map[string]any{"id": id, "parentThreadId": "thr-shared"})
	}
	server.writeResult(connection, message.id, map[string]any{"data": data, "nextCursor": nil})
}

func (server *handoffAppServer) handleArchive(connection *websocket.Conn, message rpcMessage) {
	server.mu.Lock()
	server.archiveCalls++
	if server.archiveNotReadyFailures > 0 {
		server.archiveNotReadyFailures--
		server.mu.Unlock()
		server.writeError(connection, message.id, -32600, "no rollout found")
		return
	}
	server.systemError = false
	server.subscribers = make(map[int]*websocket.Conn)
	clients := server.clientConnectionsLocked()
	ids := append([]string(nil), server.descendants...)
	ids = append(ids, "thr-shared")
	for _, id := range ids {
		server.archived[id] = true
	}
	server.mu.Unlock()
	for _, id := range ids {
		server.broadcastNotification(clients, "thread/archived", map[string]any{"threadId": id})
	}
	server.writeResult(connection, message.id, map[string]any{})
}

func (server *handoffAppServer) handleUnarchive(connection *websocket.Conn, message rpcMessage) {
	threadID, _ := requireThreadID(message)
	server.mu.Lock()
	if server.failUnarchive == threadID {
		server.mu.Unlock()
		server.writeError(connection, message.id, -32002, "unarchive failed")
		return
	}
	if !server.archived[threadID] {
		server.mu.Unlock()
		server.writeError(connection, message.id, -32600, "no archived rollout found for thread id "+threadID)
		return
	}
	delete(server.archived, threadID)
	server.unarchived = append(server.unarchived, threadID)
	if threadID == "thr-shared" {
		server.softReloaded = true
	}
	clients := server.clientConnectionsLocked()
	server.mu.Unlock()
	server.broadcastNotification(clients, "thread/unarchived", map[string]any{"threadId": threadID})
	server.writeResult(connection, message.id, map[string]any{"thread": map[string]any{"id": threadID}})
}

func (server *handoffAppServer) handleUnsubscribe(connection *websocket.Conn, clientID int, message rpcMessage) {
	server.mu.Lock()
	_, subscribed := server.subscribers[clientID]
	delete(server.subscribers, clientID)
	server.mu.Unlock()
	status := "notSubscribed"
	if subscribed {
		status = "unsubscribed"
	}
	server.writeResult(connection, message.id, map[string]any{"status": status})
}

func (server *handoffAppServer) handleTurnStart(connection *websocket.Conn, message rpcMessage) {
	threadID, _ := requireThreadID(message)
	server.mu.Lock()
	server.turnStarts++
	if server.active {
		server.mu.Unlock()
		server.writeError(connection, message.id, -32001, "turn already active")
		return
	}
	server.active = true
	provider := server.provider
	subscribers := server.subscriberConnectionsLocked()
	startedGate := server.startedGate
	server.mu.Unlock()
	server.turns <- handoffTurnRecord{threadID: threadID, provider: provider}
	server.writeResult(connection, message.id, map[string]any{
		"turn": map[string]any{"id": "turn-test", "status": "inProgress", "items": []any{}},
	})
	if startedGate != nil {
		select {
		case <-startedGate:
		case <-server.ctx.Done():
			return
		}
	}
	server.broadcastNotification(subscribers, "turn/started", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": "turn-test", "status": "inProgress", "items": []any{}},
	})
	if server.autoComplete {
		server.mu.Lock()
		server.active = false
		subscribers = server.subscriberConnectionsLocked()
		server.mu.Unlock()
		server.broadcastNotification(subscribers, "turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": "turn-test", "status": "completed", "items": []any{}},
		})
	}
}

func (server *handoffAppServer) subscriberCount() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.subscribers)
}

func (server *handoffAppServer) turnStartCallCount() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.turnStarts
}

func (server *handoffAppServer) subscriberConnectionsLocked() []*websocket.Conn {
	connections := make([]*websocket.Conn, 0, len(server.subscribers))
	for _, connection := range server.subscribers {
		connections = append(connections, connection)
	}
	return connections
}

func (server *handoffAppServer) clientConnectionsLocked() []*websocket.Conn {
	connections := make([]*websocket.Conn, 0, len(server.clients))
	for _, connection := range server.clients {
		connections = append(connections, connection)
	}
	return connections
}

func (server *handoffAppServer) writeResult(connection *websocket.Conn, id json.RawMessage, result any) {
	server.writeJSON(connection, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (server *handoffAppServer) writeNotification(connection *websocket.Conn, method string, params any) {
	server.writeJSON(connection, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (server *handoffAppServer) broadcastNotification(connections []*websocket.Conn, method string, params any) {
	for _, connection := range connections {
		server.writeNotification(connection, method, params)
	}
}

func (server *handoffAppServer) writeError(connection *websocket.Conn, id json.RawMessage, code int, message string) {
	server.writeJSON(connection, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (server *handoffAppServer) writeJSON(connection *websocket.Conn, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	server.writeMu.Lock()
	defer server.writeMu.Unlock()
	_ = connection.Write(server.ctx, websocket.MessageText, payload)
}

type visibleRPC struct {
	id           string
	method       string
	delta        string
	errorCode    int
	errorMessage string
}

func dialProviderProxy(t *testing.T, ctx context.Context, socket, provider string) (*websocket.Conn, <-chan error) {
	return dialProviderProxyOptions(t, ctx, config.Config{Provider: provider, Socket: socket})
}

func dialProviderProxyOptions(t *testing.T, ctx context.Context, configuration config.Config) (*websocket.Conn, <-chan error) {
	t.Helper()
	clientStream, switcherStream := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Config: configuration,
			Stdin:  switcherStream,
			Stdout: switcherStream,
		})
	}()
	client, response, err := websocket.Dial(ctx, "ws://desktop.test/", &websocket.DialOptions{
		HTTPClient: pipeHTTPClient(clientStream),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d", response.StatusCode)
	}
	return client, done
}

func dialRawAppServer(t *testing.T, ctx context.Context, socket string) *websocket.Conn {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(dialContext context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(dialContext, "unix", socket)
		},
	}}
	connection, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func initializeTestClient(t *testing.T, ctx context.Context, connection *websocket.Conn) {
	t.Helper()
	sendRPC(t, ctx, connection, 1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "handoff-test", "version": "1"},
	})
	readResponse(t, ctx, connection, `1`)
	if err := connection.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized"}`)); err != nil {
		t.Fatal(err)
	}
}

func resumeTestThread(t *testing.T, ctx context.Context, connection *websocket.Conn, id int) string {
	t.Helper()
	sendRPC(t, ctx, connection, id, "thread/resume", map[string]any{"threadId": "thr-shared"})
	response := readResponse(t, ctx, connection, fmt.Sprint(id))
	return responseProvider(t, response.raw)
}

func sendTestTurnAndCollect(t *testing.T, ctx context.Context, connection *websocket.Conn, id int) []visibleRPC {
	t.Helper()
	sendRPC(t, ctx, connection, id, "turn/start", map[string]any{
		"threadId": "thr-shared",
		"input":    []any{},
	})
	var visible []visibleRPC
	for {
		message := readVisibleRPC(t, ctx, connection)
		visible = append(visible, message)
		if message.method == "turn/completed" {
			return visible
		}
	}
}

func readProviderControlLifecycle(
	t *testing.T,
	ctx context.Context,
	connection *websocket.Conn,
	id string,
) (bool, []string, string) {
	t.Helper()
	methods := make([]string, 0, 7)
	responseSeen := false
	feedback := ""
	for len(methods) < 7 {
		message := readVisibleRPC(t, ctx, connection)
		if message.id == id {
			responseSeen = true
		}
		if message.method != "" {
			methods = append(methods, message.method)
		}
		if message.method == "item/agentMessage/delta" {
			feedback = message.delta
		}
	}
	wantMethods := []string{
		"turn/started",
		"item/started",
		"item/completed",
		"item/started",
		"item/agentMessage/delta",
		"item/completed",
		"turn/completed",
	}
	if fmt.Sprint(methods) != fmt.Sprint(wantMethods) {
		t.Fatalf("synthetic lifecycle methods=%v, want %v", methods, wantMethods)
	}
	return responseSeen, methods, feedback
}

func sendRPC(t *testing.T, ctx context.Context, connection *websocket.Conn, id int, method string, params any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

type testRPCResponse struct {
	raw          []byte
	errorCode    int
	errorMessage string
}

func readResponse(t *testing.T, ctx context.Context, connection *websocket.Conn, id string) testRPCResponse {
	t.Helper()
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.MessageText {
			continue
		}
		parsed, err := parseRPCMessage(payload)
		if err != nil || parsed.kind != rpcResponse || parsed.idKey != id {
			continue
		}
		response := testRPCResponse{raw: payload}
		var envelope struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(payload, &envelope)
		response.errorCode = envelope.Error.Code
		response.errorMessage = envelope.Error.Message
		return response
	}
}

func readUntilMethod(t *testing.T, ctx context.Context, connection *websocket.Conn, method string) {
	t.Helper()
	for {
		visible := readVisibleRPC(t, ctx, connection)
		if visible.method == method {
			return
		}
	}
}

func readVisibleRPC(t *testing.T, ctx context.Context, connection *websocket.Conn) visibleRPC {
	t.Helper()
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.MessageText {
			continue
		}
		parsed, err := parseRPCMessage(payload)
		if err != nil {
			continue
		}
		visible := visibleRPC{id: parsed.idKey, method: parsed.method}
		if parsed.method == "item/agentMessage/delta" {
			_ = json.Unmarshal(parsed.params["delta"], &visible.delta)
		}
		if parsed.hasError {
			var envelope struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(payload, &envelope)
			visible.errorCode = envelope.Error.Code
			visible.errorMessage = envelope.Error.Message
		}
		return visible
	}
}

func assertNoVisibleMessageWithin(
	t *testing.T,
	ctx context.Context,
	connection *websocket.Conn,
	duration time.Duration,
) {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	messageType, payload, err := connection.Read(readCtx)
	if err == nil {
		t.Fatalf("unexpected visible message type=%v payload=%s", messageType, payload)
	}
	if readCtx.Err() == nil {
		t.Fatalf("connection ended before visibility timeout: %v", err)
	}
}

func responseProvider(t *testing.T, payload []byte) string {
	t.Helper()
	var response struct {
		Result struct {
			ModelProvider string `json:"modelProvider"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	return response.Result.ModelProvider
}

func assertNoInternalMessages(t *testing.T, messages []visibleRPC) {
	t.Helper()
	for _, message := range messages {
		if stringsHasPrefix(message.id, `"cps-`) || message.method == "thread/unsubscribe" {
			t.Fatalf("internal message leaked to Desktop: %#v", message)
		}
	}
}

func waitProxyDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop")
	}
}
