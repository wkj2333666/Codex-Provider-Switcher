package rollout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

var ErrCompressedHistory = errors.New("compressed history restoration failed")

// ResolvePath follows Codex's plain-before-compressed representation rule while
// preserving the same directory and symlink checks as ordinary sanitation.
func ResolvePath(home, threadID, path string) (string, error) {
	plain := strings.TrimSuffix(path, ".zst")
	if !strings.HasSuffix(path, ".jsonl.zst") {
		plain = path
	}
	if err := ValidatePath(home, threadID, plain); err == nil {
		return plain, nil
	}
	if _, err := os.Lstat(plain); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("invalid rollout path")
	}
	if !strings.HasSuffix(plain, ".jsonl") {
		return "", errors.New("invalid rollout path")
	}
	compressed := plain + ".zst"
	if err := ValidatePath(home, threadID, compressed); err != nil {
		return "", err
	}
	return compressed, nil
}

// PreparePlain restores a cold compressed history before provider sanitation.
// It shares Codex's maintenance and writer locks, publishes without clobbering
// a concurrent plain representation, and only removes the source after fsync.
// zstd is required only when a compressed history actually needs restoration.
func PreparePlain(ctx context.Context, home, threadID, path, lockPath string) (string, error) {
	resolved, err := ResolvePath(home, threadID, path)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(resolved, ".jsonl.zst") {
		return resolved, nil
	}
	plain := strings.TrimSuffix(resolved, ".zst")
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := ensureLockDirectory(filepath.Join(home, ".tmp")); err != nil {
		return "", err
	}
	maintenance, err := openLockFile(filepath.Join(home, ".tmp", "rollout-maintenance.lock"))
	if err != nil {
		return "", fmt.Errorf("%w: maintenance lock unavailable", ErrCompressedHistory)
	}
	defer maintenance.Close()
	if err := syscall.Flock(int(maintenance.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return "", fmt.Errorf("%w: history maintenance is running; retry shortly", ErrCompressedHistory)
	}
	defer syscall.Flock(int(maintenance.Fd()), syscall.LOCK_UN)
	unlock, err := acquireWriterLock(lockPath)
	if err != nil {
		return "", err
	}
	defer unlock()
	resolved, err = ResolvePath(home, threadID, path)
	if err != nil {
		return "", err
	}
	if resolved == plain {
		return plain, nil
	}
	input, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("%w: open source", ErrCompressedHistory)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: invalid source", ErrCompressedHistory)
	}
	stamp := sanitationStamp(info, resolved, threadID)
	temporary, err := os.CreateTemp(filepath.Dir(plain), ".rollout-decompress-*")
	if err != nil {
		return "", fmt.Errorf("%w: create staging file", ErrCompressedHistory)
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	command := exec.CommandContext(ctx, "zstd", "-q", "-d", "-c")
	command.Stdin = input
	command.Stdout = temporary
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%w: zstd decode failed (check zstd availability, file integrity and free space)", ErrCompressedHistory)
	}
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("%w: sync restored history", ErrCompressedHistory)
	}
	after, err := os.Lstat(resolved)
	if err != nil || stamp == "" || sanitationStamp(after, resolved, threadID) != stamp {
		return "", fmt.Errorf("%w: source changed", ErrCompressedHistory)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Link(temporary.Name(), plain); err != nil {
		return "", fmt.Errorf("%w: publish restored history", ErrCompressedHistory)
	}
	dir, err := os.Open(filepath.Dir(plain))
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return "", err
	}
	// The published plain history is now durable. A crash before unlink merely
	// leaves a redundant compressed sibling, which native Codex ignores.
	if err := os.Remove(resolved); err != nil {
		return "", err
	}
	if err := dir.Sync(); err != nil {
		return "", err
	}
	return plain, nil
}
