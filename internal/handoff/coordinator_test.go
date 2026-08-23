package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
)

func TestRuntimeDirectoryIsShortAndSocketSpecific(t *testing.T) {
	t.Parallel()
	first := runtimeDirectory(1000, "/home/user/.codex/app-server-control/app-server-control.sock")
	second := runtimeDirectory(1000, "/tmp/other.sock")
	if first == second || !strings.HasPrefix(first, "/tmp/cps-1000-") || len(first) > 64 {
		t.Fatalf("runtime directories = %q, %q", first, second)
	}
}

func TestThreadLockSerializesAndHonorsCancellation(t *testing.T) {
	coordinator := openTestCoordinator(t, &testHandler{prepare: StatusReady})
	release, err := coordinator.LockThread(context.Background(), "thr-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := coordinator.LockThread(ctx, "thr-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v", err)
	}
}

func TestPrepareAllAbortsBeforeUnsubscribeWhenPeerBusy(t *testing.T) {
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	readyOne := &testHandler{prepare: StatusReady}
	busy := &testHandler{prepare: StatusBusy}
	readyTwo := &testHandler{prepare: StatusReady}
	coordinators := []*Coordinator{
		openTestCoordinatorForSocket(t, appSocket, readyOne),
		openTestCoordinatorForSocket(t, appSocket, busy),
		openTestCoordinatorForSocket(t, appSocket, readyTwo),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinators[0].PrepareAll(ctx, "thr-a"); err == nil {
		t.Fatal("PrepareAll() error = nil")
	}
	for index, handler := range []*testHandler{readyOne, busy, readyTwo} {
		if got := handler.unsubscribeCalls(); got != 0 {
			t.Fatalf("handler %d unsubscribe calls = %d", index, got)
		}
	}
}

func TestReconcileIdleRequestClearsEveryPeer(t *testing.T) {
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	handlers := []*testHandler{
		{prepare: StatusBusy},
		{prepare: StatusBusy},
	}
	coordinators := []*Coordinator{
		openTestCoordinatorForSocket(t, appSocket, handlers[0]),
		openTestCoordinatorForSocket(t, appSocket, handlers[1]),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinators[0].ReconcileIdleAll(ctx, "thr-a"); err != nil {
		t.Fatalf("reconcile idle peers: %v", err)
	}
	if err := coordinators[0].PrepareAll(ctx, "thr-a"); err != nil {
		t.Fatalf("prepare reconciled peers: %v", err)
	}
	for index, handler := range handlers {
		if got := handler.reconcileCallCount(); got != 1 {
			t.Fatalf("handler %d reconcile calls = %d, want 1", index, got)
		}
	}
}

func TestReconcileIdleFailsClosedWithLegacyPeer(t *testing.T) {
	coordinator := openTestCoordinator(t, &testHandler{prepare: StatusBusy})
	legacyPath := filepath.Join(coordinator.directory, "session-legacy.sock")
	listener, err := net.Listen("unix", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(legacyPath)
	})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = readControlMessage(connection)
		encoded, _ := json.Marshal(controlResponse{Error: true})
		_, _ = connection.Write(append(encoded, '\n'))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinator.ReconcileIdleAll(ctx, "thr-a"); err == nil {
		t.Fatal("ReconcileIdleAll() accepted a legacy peer")
	}
}

func TestPrepareHandoffAllRequiresRouteCapablePeerBeforeMutation(t *testing.T) {
	coordinator := openTestCoordinator(t, &testHandler{prepare: StatusReady})
	legacyPath := filepath.Join(coordinator.directory, "session-legacy.sock")
	listener, err := net.Listen("unix", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(legacyPath)
	})
	methodSeen := make(chan string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		data, readErr := readControlMessage(connection)
		if readErr != nil {
			return
		}
		var request controlRequest
		if json.Unmarshal(data, &request) != nil {
			return
		}
		methodSeen <- request.Method
		response := controlResponse{Status: StatusReady}
		if request.Method != "prepare" {
			response = controlResponse{Error: true}
		}
		encoded, _ := json.Marshal(response)
		_, _ = connection.Write(append(encoded, '\n'))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinator.PrepareHandoffAll(ctx, "thr-a"); err == nil {
		t.Fatal("PrepareHandoffAll() error = nil with legacy peer")
	}
	select {
	case method := <-methodSeen:
		if method != "prepareHandoffV3" {
			t.Fatalf("legacy peer method = %q", method)
		}
	case <-ctx.Done():
		t.Fatal("legacy peer did not receive capability probe")
	}
}

func TestPrepareRecoveryAllRequiresRecoveryCapablePeer(t *testing.T) {
	coordinator := openTestCoordinator(t, &testHandler{prepare: StatusReady})
	legacyPath := filepath.Join(coordinator.directory, "session-handoff-v2.sock")
	listener, err := net.Listen("unix", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(legacyPath)
	})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		data, readErr := readControlMessage(connection)
		if readErr != nil {
			return
		}
		var request controlRequest
		_ = json.Unmarshal(data, &request)
		response := controlResponse{Error: request.Method == "prepareRecoveryV1", Status: StatusReady}
		encoded, _ := json.Marshal(response)
		_, _ = connection.Write(append(encoded, '\n'))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinator.PrepareRecoveryAll(ctx, "thr-a"); err == nil {
		t.Fatal("PrepareRecoveryAll() error = nil with handoff-v2 peer")
	}
}

func TestDirtyStateIsSharedAcrossCoordinators(t *testing.T) {
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	first := openTestCoordinatorForSocket(t, appSocket, &testHandler{prepare: StatusReady})
	second := openTestCoordinatorForSocket(t, appSocket, &testHandler{prepare: StatusReady})

	if first.IsDirty("thr-a") || second.IsDirty("thr-a") {
		t.Fatal("new thread unexpectedly dirty")
	}
	if err := first.MarkDirty("thr-a"); err != nil {
		t.Fatal(err)
	}
	assertDirtyStage(t, first, "thr-a", DirtyStagePrepared)
	if !second.IsDirty("thr-a") {
		t.Fatal("dirty marker not visible to second coordinator")
	}
	if err := second.SetDirtyStage("thr-a", DirtyStageUnsubscribed); err != nil {
		t.Fatal(err)
	}
	assertDirtyStage(t, first, "thr-a", DirtyStageUnsubscribed)
	if err := second.ClearDirty("thr-a"); err != nil {
		t.Fatal(err)
	}
	if first.IsDirty("thr-a") {
		t.Fatal("cleared dirty marker still visible")
	}
}

func TestDirtyStageRejectsInvalidValueAndReplacesLegacyMarker(t *testing.T) {
	coordinator := openTestCoordinator(t, &testHandler{prepare: StatusReady})
	path := coordinator.threadStatePath("dirty", "thr-a", ".state")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !coordinator.IsDirty("thr-a") {
		t.Fatal("legacy empty marker is not dirty")
	}
	if err := coordinator.SetDirtyStage("thr-a", DirtyStage("invalid")); err == nil {
		t.Fatal("SetDirtyStage() error = nil for invalid stage")
	}
	if err := coordinator.SetDirtyStage("thr-a", DirtyStageResumeMismatch); err != nil {
		t.Fatal(err)
	}
	assertDirtyStage(t, coordinator, "thr-a", DirtyStageResumeMismatch)
}

func assertDirtyStage(t *testing.T, coordinator *Coordinator, threadID string, want DirtyStage) {
	t.Helper()
	path := coordinator.threadStatePath("dirty", threadID, ".state")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record dirtyRecord
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.Stage != want {
		t.Fatalf("dirty record = %q, %#v; want stage %q", data, record, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("dirty marker mode = %o, want 600", info.Mode().Perm())
	}
}

func TestUnsubscribeAllContactsEveryLivePeer(t *testing.T) {
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	handlers := []*testHandler{
		{prepare: StatusReady, unsubscribe: StatusUnsubscribed},
		{prepare: StatusReady, unsubscribe: StatusNotSubscribed},
		{prepare: StatusReady, unsubscribe: StatusNotLoaded},
	}
	coordinators := make([]*Coordinator, 0, len(handlers))
	for _, handler := range handlers {
		coordinators = append(coordinators, openTestCoordinatorForSocket(t, appSocket, handler))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinators[0].PrepareAll(ctx, "thr-a"); err != nil {
		t.Fatalf("PrepareAll() error = %v", err)
	}
	if err := coordinators[0].UnsubscribeAll(ctx, "thr-a"); err != nil {
		t.Fatalf("UnsubscribeAll() error = %v", err)
	}
	for index, handler := range handlers {
		if got := handler.unsubscribeCalls(); got != 1 {
			t.Fatalf("handler %d unsubscribe calls = %d", index, got)
		}
	}
}

func TestResubscribeAllRestoresEveryDetachedPeerWithEffectiveRoute(t *testing.T) {
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	handlers := []*testHandler{
		{prepare: StatusReady, resubscribe: StatusResubscribed},
		{prepare: StatusReady, resubscribe: StatusNotSubscribed},
	}
	coordinators := make([]*Coordinator, 0, len(handlers))
	for _, handler := range handlers {
		coordinators = append(coordinators, openTestCoordinatorForSocket(t, appSocket, handler))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wantRoute := modelroute.Route{Provider: "glm", Model: "glm-5.2"}
	if err := coordinators[0].ResubscribeAll(ctx, "thr-a", wantRoute); err != nil {
		t.Fatalf("ResubscribeAll() error = %v", err)
	}
	for index, handler := range handlers {
		calls, route := handler.resubscribeResult()
		if calls != 1 || route != wantRoute {
			t.Fatalf("handler %d resubscribe = %d, %#v", index, calls, route)
		}
	}
}

func TestResubscribeAllRejectsInvalidRouteBeforeContactingPeers(t *testing.T) {
	handler := &testHandler{prepare: StatusReady, resubscribe: StatusResubscribed}
	coordinator := openTestCoordinator(t, handler)
	for _, route := range []modelroute.Route{
		{},
		{Provider: "bad provider"},
		{Provider: "glm", Model: "bad model"},
	} {
		if err := coordinator.ResubscribeAll(context.Background(), "thr-a", route); err == nil {
			t.Fatalf("ResubscribeAll(%#v) error = nil", route)
		}
	}
	if calls, _ := handler.resubscribeResult(); calls != 0 {
		t.Fatalf("invalid routes contacted peer %d times", calls)
	}
}

func TestRestoreAllVisitsEveryPeerAfterFailure(t *testing.T) {
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	handlers := []*testHandler{
		{prepare: StatusReady, restore: StatusRestored},
		{prepare: StatusReady, restoreErr: errors.New("unavailable")},
		{prepare: StatusReady, restore: StatusNotSubscribed},
	}
	coordinators := make([]*Coordinator, 0, len(handlers))
	for _, handler := range handlers {
		coordinators = append(coordinators, openTestCoordinatorForSocket(t, appSocket, handler))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinators[0].RestoreAll(ctx, "thr-a"); err == nil {
		t.Fatal("RestoreAll() error = nil")
	}
	for index, handler := range handlers {
		if calls := handler.restoreCallCount(); calls != 1 {
			t.Fatalf("handler %d restore calls = %d", index, calls)
		}
	}
}

func TestRecoveryModeIsAcknowledgedAndClearedByEveryPeer(t *testing.T) {
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	handlers := []*testHandler{
		{prepare: StatusReady},
		{prepare: StatusReady},
	}
	coordinators := []*Coordinator{
		openTestCoordinatorForSocket(t, appSocket, handlers[0]),
		openTestCoordinatorForSocket(t, appSocket, handlers[1]),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ids := []string{"child-a", "thr-a"}
	if err := coordinators[0].BeginRecoveryAll(ctx, "thr-a", ids); err != nil {
		t.Fatal(err)
	}
	if err := coordinators[0].EndRecoveryAll(ctx, "thr-a"); err != nil {
		t.Fatal(err)
	}
	for index, handler := range handlers {
		begin, end, gotIDs := handler.recoveryResult()
		if begin != 1 || end != 1 || !reflect.DeepEqual(gotIDs, ids) {
			t.Fatalf("handler %d recovery = %d, %d, %v", index, begin, end, gotIDs)
		}
	}
}

func TestRecoveryModeRejectsInvalidIDSetBeforeContactingPeers(t *testing.T) {
	handler := &testHandler{prepare: StatusReady}
	coordinator := openTestCoordinator(t, handler)
	ctx := context.Background()
	invalid := [][]string{nil, {"other"}, {"thr-a", "thr-a"}}
	oversized := make([]string, 65)
	for index := range oversized {
		oversized[index] = fmt.Sprintf("thr-%d", index)
	}
	invalid = append(invalid, oversized)
	for _, ids := range invalid {
		if err := coordinator.BeginRecoveryAll(ctx, "thr-a", ids); err == nil {
			t.Fatalf("BeginRecoveryAll(%v) error = nil", ids)
		}
	}
	begin, _, _ := handler.recoveryResult()
	if begin != 0 {
		t.Fatalf("invalid recovery contacted peer %d times", begin)
	}
}

func TestBeginRecoveryRollsBackEveryPeerAfterPartialFailure(t *testing.T) {
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	handlers := []*testHandler{
		{prepare: StatusReady},
		{prepare: StatusReady, recoveryBeginStatus: StatusBusy},
		{prepare: StatusReady},
	}
	coordinators := make([]*Coordinator, 0, len(handlers))
	for _, handler := range handlers {
		coordinators = append(coordinators, openTestCoordinatorForSocket(t, appSocket, handler))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinators[0].BeginRecoveryAll(ctx, "thr-a", []string{"thr-a"}); err == nil {
		t.Fatal("BeginRecoveryAll() error = nil after peer rejection")
	}
	for index, handler := range handlers {
		_, end, _ := handler.recoveryResult()
		if end != 1 {
			t.Fatalf("handler %d rollback end calls = %d, want 1", index, end)
		}
	}
}

func TestPrepareAllRemovesStaleSocket(t *testing.T) {
	coordinator := openTestCoordinator(t, &testHandler{prepare: StatusReady})
	stalePath := filepath.Join(coordinator.directory, "session-stale.sock")
	address, err := net.ResolveUnixAddr("unix", stalePath)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stalePath); err != nil {
		t.Fatalf("create stale socket: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinator.PrepareAll(ctx, "thr-a"); err != nil {
		t.Fatalf("PrepareAll() error = %v", err)
	}
	if _, err := os.Lstat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket still exists: %v", err)
	}
}

type testHandler struct {
	mu                  sync.Mutex
	prepare             PeerStatus
	unsubscribe         PeerStatus
	resubscribe         PeerStatus
	restore             PeerStatus
	calls               int
	resumeCalls         int
	restoreCalls        int
	route               modelroute.Route
	restoreErr          error
	recoveryBegin       int
	recoveryEnd         int
	recoveryIDs         []string
	recoveryBeginStatus PeerStatus
	reconcileCalls      int
}

func (handler *testHandler) BeginRecovery(_ string, ids []string) PeerStatus {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.recoveryBegin++
	handler.recoveryIDs = append([]string(nil), ids...)
	if handler.recoveryBeginStatus != "" {
		return handler.recoveryBeginStatus
	}
	return StatusRecoveryReady
}

func (handler *testHandler) EndRecovery(string) PeerStatus {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.recoveryEnd++
	return StatusRecoveryEnded
}

func (handler *testHandler) Prepare(string) PeerStatus {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.prepare
}

func (handler *testHandler) ReconcileIdle(string) PeerStatus {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.reconcileCalls++
	handler.prepare = StatusReady
	return StatusReady
}

func (handler *testHandler) Unsubscribe(context.Context, string) (PeerStatus, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.calls++
	return handler.unsubscribe, nil
}

func (handler *testHandler) Resubscribe(_ context.Context, _ string, route modelroute.Route) (PeerStatus, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.resumeCalls++
	handler.route = route
	return handler.resubscribe, nil
}

func (handler *testHandler) Restore(context.Context, string) (PeerStatus, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.restoreCalls++
	return handler.restore, handler.restoreErr
}

func (handler *testHandler) unsubscribeCalls() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.calls
}

func (handler *testHandler) resubscribeResult() (int, modelroute.Route) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.resumeCalls, handler.route
}

func (handler *testHandler) restoreCallCount() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.restoreCalls
}

func (handler *testHandler) recoveryResult() (int, int, []string) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.recoveryBegin, handler.recoveryEnd, append([]string(nil), handler.recoveryIDs...)
}

func (handler *testHandler) reconcileCallCount() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.reconcileCalls
}

func openTestCoordinator(t *testing.T, handler Handler) *Coordinator {
	t.Helper()
	return openTestCoordinatorForSocket(t, filepath.Join(t.TempDir(), "app-server.sock"), handler)
}

func openTestCoordinatorForSocket(t *testing.T, appSocket string, handler Handler) *Coordinator {
	t.Helper()
	coordinator, err := Open(appSocket, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = coordinator.Close()
		_ = os.RemoveAll(coordinator.directory)
	})
	return coordinator
}
