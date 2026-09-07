package handoff

import (
	"context"
	"testing"
	"time"

	"github.com/wkj2333666/Codex-Provider-Switcher/internal/modelroute"
)

type slowResumeHandler struct {
	testHandler
	started chan struct{}
}

func (handler *slowResumeHandler) Resubscribe(ctx context.Context, _ string, _ modelroute.Route) (PeerStatus, error) {
	if handler.started != nil {
		close(handler.started)
	}
	select {
	case <-time.After(2200 * time.Millisecond):
		return StatusResubscribed, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestSlowPeerHonorsCancellationWithoutDeadline(t *testing.T) {
	started := make(chan struct{})
	coordinator := openTestCoordinator(t, &slowResumeHandler{started: started})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := callPeer(ctx, coordinator.socketPath, controlRequest{Method: "resubscribe", ThreadID: "thr-a", Provider: "openai"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled peer request succeeded")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled caller remained blocked on peer")
	}
}

func TestPeerAllowsSlowMetadataResume(t *testing.T) {
	coordinator := openTestCoordinator(t, &slowResumeHandler{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, _, err := callPeer(ctx, coordinator.socketPath, controlRequest{Method: "resubscribe", ThreadID: "thr-a", Provider: "openai"})
	if err != nil || status != StatusResubscribed {
		t.Fatalf("slow resume = %q, %v", status, err)
	}
}

func TestSlowPeerHonorsCallerDeadline(t *testing.T) {
	coordinator := openTestCoordinator(t, &slowResumeHandler{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := callPeer(ctx, coordinator.socketPath, controlRequest{Method: "resubscribe", ThreadID: "thr-a", Provider: "openai"}); err == nil {
		t.Fatal("caller deadline ignored")
	}
}
