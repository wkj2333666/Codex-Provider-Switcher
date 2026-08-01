package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestPrepareHandoffAllRejectsLegacyPeerBeforeMutation(t *testing.T) {
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
		if method != "prepareHandoffV2" {
			t.Fatalf("legacy peer method = %q", method)
		}
	case <-ctx.Done():
		t.Fatal("legacy peer did not receive capability probe")
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
	if !second.IsDirty("thr-a") {
		t.Fatal("dirty marker not visible to second coordinator")
	}
	if err := second.ClearDirty("thr-a"); err != nil {
		t.Fatal(err)
	}
	if first.IsDirty("thr-a") {
		t.Fatal("cleared dirty marker still visible")
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

func TestResubscribeAllRestoresEveryDetachedPeerWithEffectiveProvider(t *testing.T) {
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
	if err := coordinators[0].ResubscribeAll(ctx, "thr-a", "sub2api"); err != nil {
		t.Fatalf("ResubscribeAll() error = %v", err)
	}
	for index, handler := range handlers {
		calls, provider := handler.resubscribeResult()
		if calls != 1 || provider != "sub2api" {
			t.Fatalf("handler %d resubscribe = %d, %q", index, calls, provider)
		}
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
	mu           sync.Mutex
	prepare      PeerStatus
	unsubscribe  PeerStatus
	resubscribe  PeerStatus
	restore      PeerStatus
	calls        int
	resumeCalls  int
	restoreCalls int
	provider     string
	restoreErr   error
}

func (handler *testHandler) Prepare(string) PeerStatus {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.prepare
}

func (handler *testHandler) Unsubscribe(context.Context, string) (PeerStatus, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.calls++
	return handler.unsubscribe, nil
}

func (handler *testHandler) Resubscribe(_ context.Context, _, provider string) (PeerStatus, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.resumeCalls++
	handler.provider = provider
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

func (handler *testHandler) resubscribeResult() (int, string) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.resumeCalls, handler.provider
}

func (handler *testHandler) restoreCallCount() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.restoreCalls
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
