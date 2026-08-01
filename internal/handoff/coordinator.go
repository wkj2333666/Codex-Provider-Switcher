// Package handoff coordinates provider ownership changes across live proxy
// processes connected to the same Codex app-server socket.
package handoff

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	controlMessageLimit = 4 << 10
	peerTimeout         = 2 * time.Second
	lockRetryInterval   = 10 * time.Millisecond
)

// PeerStatus is a bounded result from one cooperating proxy connection.
type PeerStatus string

const (
	StatusReady         PeerStatus = "ready"
	StatusBusy          PeerStatus = "busy"
	StatusUnsubscribed  PeerStatus = "unsubscribed"
	StatusNotSubscribed PeerStatus = "notSubscribed"
	StatusNotLoaded     PeerStatus = "notLoaded"
	StatusResubscribed  PeerStatus = "resubscribed"
	StatusRestored      PeerStatus = "restored"
)

// Handler applies peer requests to one proxy connection.
type Handler interface {
	Prepare(threadID string) PeerStatus
	Unsubscribe(context.Context, string) (PeerStatus, error)
	Resubscribe(context.Context, string, string) (PeerStatus, error)
	Restore(context.Context, string) (PeerStatus, error)
}

// Coordinator exposes one peer endpoint and discovers other live endpoints in
// the runtime namespace for the same app-server socket.
type Coordinator struct {
	directory  string
	socketPath string
	listener   net.Listener
	handler    Handler
	closeOnce  sync.Once
	done       chan struct{}
}

type controlRequest struct {
	Method   string `json:"method"`
	ThreadID string `json:"threadId"`
	Provider string `json:"provider,omitempty"`
}

type controlResponse struct {
	Status PeerStatus `json:"status,omitempty"`
	Error  bool       `json:"error,omitempty"`
}

// Open registers a live coordinator peer for one proxy connection.
func Open(appServerSocket string, handler Handler) (*Coordinator, error) {
	if appServerSocket == "" || handler == nil {
		return nil, errors.New("invalid handoff coordinator configuration")
	}
	directory := runtimeDirectory(os.Getuid(), appServerSocket)
	if err := ensureRuntimeDirectory(directory); err != nil {
		return nil, err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return nil, err
	}
	socketPath := filepath.Join(directory, "session-"+suffix+".sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, errors.New("listen for provider handoff peers")
	}
	coordinator := &Coordinator{
		directory:  directory,
		socketPath: socketPath,
		listener:   listener,
		handler:    handler,
		done:       make(chan struct{}),
	}
	go coordinator.serve()
	return coordinator, nil
}

// LockThread takes the cross-process transition lock for a thread.
func (coordinator *Coordinator) LockThread(ctx context.Context, threadID string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if threadID == "" {
		return nil, errors.New("invalid thread handoff lock")
	}
	path := coordinator.threadStatePath("lock", threadID, ".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("open thread handoff lock")
	}

	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
					_ = file.Close()
				})
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, errors.New("acquire thread handoff lock")
		}

		timer := time.NewTimer(lockRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// PrepareAll verifies that every live peer can release the target thread
// without changing subscription state.
func (coordinator *Coordinator) PrepareAll(ctx context.Context, threadID string) error {
	return coordinator.visitPeers(ctx, controlRequest{Method: "prepare", ThreadID: threadID}, func(status PeerStatus) bool {
		return status == StatusReady
	})
}

// PrepareHandoffAll verifies that every peer supports subscription restoration
// before the first unsubscribe mutates app-server state.
func (coordinator *Coordinator) PrepareHandoffAll(ctx context.Context, threadID string) error {
	return coordinator.visitPeers(ctx, controlRequest{Method: "prepareHandoffV2", ThreadID: threadID}, func(status PeerStatus) bool {
		return status == StatusReady
	})
}

// MarkDirty persists incomplete handoff state across every live proxy process.
func (coordinator *Coordinator) MarkDirty(threadID string) error {
	if threadID == "" {
		return errors.New("invalid dirty handoff thread")
	}
	file, err := os.OpenFile(coordinator.threadStatePath("dirty", threadID, ".state"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("mark dirty handoff thread")
	}
	if err := file.Close(); err != nil {
		return errors.New("close dirty handoff marker")
	}
	return nil
}

// IsDirty reports whether a prior handoff may have left peer state divergent.
func (coordinator *Coordinator) IsDirty(threadID string) bool {
	if threadID == "" {
		return true
	}
	_, err := os.Lstat(coordinator.threadStatePath("dirty", threadID, ".state"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return true
}

// ClearDirty removes the marker only after every handoff phase succeeds.
func (coordinator *Coordinator) ClearDirty(threadID string) error {
	if threadID == "" {
		return errors.New("invalid dirty handoff thread")
	}
	err := os.Remove(coordinator.threadStatePath("dirty", threadID, ".state"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("clear dirty handoff thread")
	}
	return nil
}

// UnsubscribeAll removes every cooperating connection's subscription.
func (coordinator *Coordinator) UnsubscribeAll(ctx context.Context, threadID string) error {
	return coordinator.visitPeers(ctx, controlRequest{Method: "unsubscribe", ThreadID: threadID}, func(status PeerStatus) bool {
		return status == StatusUnsubscribed || status == StatusNotSubscribed || status == StatusNotLoaded
	})
}

// ResubscribeAll restores every connection detached by the handoff under the
// verified effective provider.
func (coordinator *Coordinator) ResubscribeAll(ctx context.Context, threadID, provider string) error {
	if provider == "" {
		return errors.New("invalid provider handoff resubscribe")
	}
	return coordinator.visitPeers(ctx, controlRequest{
		Method: "resubscribe", ThreadID: threadID, Provider: provider,
	}, func(status PeerStatus) bool {
		return status == StatusResubscribed || status == StatusNotSubscribed
	})
}

// RestoreAll attempts every peer even when one restore fails.
func (coordinator *Coordinator) RestoreAll(ctx context.Context, threadID string) error {
	return coordinator.visitPeersMode(ctx, controlRequest{Method: "restore", ThreadID: threadID}, func(status PeerStatus) bool {
		return status == StatusRestored || status == StatusNotSubscribed
	}, true)
}

// Close removes this peer endpoint.
func (coordinator *Coordinator) Close() error {
	var closeErr error
	coordinator.closeOnce.Do(func() {
		closeErr = coordinator.listener.Close()
		_ = os.Remove(coordinator.socketPath)
		<-coordinator.done
	})
	return closeErr
}

func (coordinator *Coordinator) serve() {
	defer close(coordinator.done)
	for {
		connection, err := coordinator.listener.Accept()
		if err != nil {
			return
		}
		go coordinator.handleConnection(connection)
	}
}

func (coordinator *Coordinator) handleConnection(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(peerTimeout))
	data, err := readControlMessage(connection)
	if err != nil {
		coordinator.writeControlResponse(connection, controlResponse{Error: true})
		return
	}
	var request controlRequest
	if json.Unmarshal(data, &request) != nil || request.ThreadID == "" {
		coordinator.writeControlResponse(connection, controlResponse{Error: true})
		return
	}

	response := controlResponse{}
	switch request.Method {
	case "prepare":
		response.Status = coordinator.handler.Prepare(request.ThreadID)
	case "prepareHandoffV2":
		response.Status = coordinator.handler.Prepare(request.ThreadID)
	case "unsubscribe":
		ctx, cancel := context.WithTimeout(context.Background(), peerTimeout)
		defer cancel()
		status, err := coordinator.handler.Unsubscribe(ctx, request.ThreadID)
		if err != nil {
			response.Error = true
		} else {
			response.Status = status
		}
	case "resubscribe":
		if request.Provider == "" {
			response.Error = true
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), peerTimeout)
		defer cancel()
		status, err := coordinator.handler.Resubscribe(ctx, request.ThreadID, request.Provider)
		if err != nil {
			response.Error = true
		} else {
			response.Status = status
		}
	case "restore":
		ctx, cancel := context.WithTimeout(context.Background(), peerTimeout)
		defer cancel()
		status, err := coordinator.handler.Restore(ctx, request.ThreadID)
		if err != nil {
			response.Error = true
		} else {
			response.Status = status
		}
	default:
		response.Error = true
	}
	coordinator.writeControlResponse(connection, response)
}

func (coordinator *Coordinator) writeControlResponse(connection net.Conn, response controlResponse) {
	encoded, err := json.Marshal(response)
	if err != nil {
		return
	}
	_, _ = connection.Write(append(encoded, '\n'))
}

func (coordinator *Coordinator) visitPeers(ctx context.Context, request controlRequest, accept func(PeerStatus) bool) error {
	return coordinator.visitPeersMode(ctx, request, accept, false)
}

func (coordinator *Coordinator) visitPeersMode(ctx context.Context, request controlRequest, accept func(PeerStatus) bool, bestEffort bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := filepath.Glob(filepath.Join(coordinator.directory, "session-*.sock"))
	if err != nil {
		return errors.New("list provider handoff peers")
	}
	sort.Strings(paths)
	failed := false
	for _, path := range paths {
		status, stale, err := callPeer(ctx, path, request)
		if stale {
			removeStaleSocket(path)
			continue
		}
		if err != nil || !accept(status) {
			failed = true
			if !bestEffort {
				return errors.New("provider handoff peer unavailable")
			}
		}
	}
	if failed {
		return errors.New("provider handoff peer unavailable")
	}
	return nil
}

func (coordinator *Coordinator) threadStatePath(prefix, threadID, suffix string) string {
	digest := sha256.Sum256([]byte(threadID))
	return filepath.Join(coordinator.directory, prefix+"-"+hex.EncodeToString(digest[:16])+suffix)
}

func callPeer(ctx context.Context, path string, request controlRequest) (PeerStatus, bool, error) {
	dialer := net.Dialer{Timeout: peerTimeout}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return "", true, nil
		}
		return "", false, errors.New("connect provider handoff peer")
	}
	defer connection.Close()
	deadline := time.Now().Add(peerTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = connection.SetDeadline(deadline)
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", false, errors.New("encode provider handoff request")
	}
	if _, err := connection.Write(append(encoded, '\n')); err != nil {
		return "", false, errors.New("send provider handoff request")
	}
	data, err := readControlMessage(connection)
	if err != nil {
		return "", false, errors.New("read provider handoff response")
	}
	var response controlResponse
	if json.Unmarshal(data, &response) != nil || response.Error || response.Status == "" {
		return "", false, errors.New("invalid provider handoff response")
	}
	return response.Status, false, nil
}

func readControlMessage(reader net.Conn) ([]byte, error) {
	buffered := bufio.NewReaderSize(reader, controlMessageLimit+1)
	data, err := buffered.ReadBytes('\n')
	if err != nil || len(data) > controlMessageLimit {
		return nil, errors.New("invalid provider handoff control message")
	}
	return data, nil
}

func removeStaleSocket(path string) {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
}

func runtimeDirectory(uid int, appServerSocket string) string {
	digest := sha256.Sum256([]byte(appServerSocket))
	return filepath.Join("/tmp", fmt.Sprintf("cps-%d-%s", uid, hex.EncodeToString(digest[:6])))
}

func ensureRuntimeDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return errors.New("create provider handoff runtime directory")
		}
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid provider handoff runtime directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return errors.New("secure provider handoff runtime directory")
	}
	return nil
}

func randomSuffix() (string, error) {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("generate provider handoff peer id")
	}
	return hex.EncodeToString(value), nil
}
