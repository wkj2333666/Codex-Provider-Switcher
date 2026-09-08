package rollout

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type sanitationCertificate struct {
	Stamp         string `json:"stamp"`
	Identity      string `json:"identity"`
	Size          int64  `json:"size"`
	Paginated     bool   `json:"paginated"`
	EndsInNewline bool   `json:"ends_in_newline"`
}

// ctime detects edits that restore size and mtime. The version invalidates
// certificates when sanitation rules change; identity binds the scan cursor to
// this file and thread even when their metadata changes after an append.
func sanitationStamp(info os.FileInfo, path, threadID string) string {
	identity := sanitationIdentity(info, path, threadID)
	if identity == "" {
		return ""
	}
	stat := info.Sys().(*syscall.Stat_t)
	value := fmt.Sprintf("%s:%d:%d:%d", identity, info.Size(), stat.Mtim.Nano(), stat.Ctim.Nano())
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func sanitationIdentity(info os.FileInfo, path, threadID string) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	value := fmt.Sprintf("v3\x00%s\x00%s\x00%d:%d", filepath.Clean(path), threadID, stat.Dev, stat.Ino)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func cachedSanitation(lockPath string) sanitationCertificate {
	if lockPath == "" || validateDirectory(filepath.Dir(lockPath)) != nil {
		return sanitationCertificate{}
	}
	file, err := os.OpenFile(lockPath+".sanitized", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return sanitationCertificate{}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 2048 {
		return sanitationCertificate{}
	}
	data, err := io.ReadAll(io.LimitReader(file, 2049))
	var certificate sanitationCertificate
	if err != nil || json.Unmarshal(data, &certificate) != nil || len(certificate.Stamp) != 64 || len(certificate.Identity) != 64 || certificate.Size < 0 {
		return sanitationCertificate{}
	}
	return certificate
}

func rememberSanitation(lockPath string, certificate sanitationCertificate) {
	if lockPath == "" || ensureLockDirectory(filepath.Dir(lockPath)) != nil {
		return
	}
	file, err := os.CreateTemp(filepath.Dir(lockPath), ".sanitation-*")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	writeErr := json.NewEncoder(file).Encode(certificate)
	closeErr := file.Close()
	if writeErr == nil && closeErr == nil {
		_ = os.Rename(file.Name(), lockPath+".sanitized")
	}
}

// tailByte records whether the inspected suffix ends at a complete JSONL line.
type tailByte struct{ last byte }

func (tail *tailByte) Write(data []byte) (int, error) {
	if len(data) > 0 {
		tail.last = data[len(data)-1]
	}
	return len(data), nil
}

func inspectSanitation(file *os.File, path, threadID string, previous sanitationCertificate) (Result, sanitationCertificate, error) {
	// The path may have been replaced after the caller's stat. Only the opened
	// descriptor can establish whether the saved cursor belongs to this file.
	info, err := file.Stat()
	if err != nil {
		return Result{}, sanitationCertificate{}, err
	}
	identity := sanitationIdentity(info, path, threadID)
	cached := int64(0)
	if previous.Identity == identity && identity != "" && previous.EndsInNewline && previous.Size > 0 && previous.Size < info.Size() {
		// Rollouts are append-only. Trust the saved cursor without reading or
		// hashing previously scanned bytes. Keep v3 identity compatibility so
		// existing hash-backed certificates also upgrade without a full scan.
		if _, err := file.Seek(previous.Size, io.SeekStart); err != nil {
			return Result{}, sanitationCertificate{}, err
		}
		cached = previous.Size
	}
	checkThread := threadID
	if cached > 0 {
		checkThread = ""
	} // The cached prefix includes session_meta.
	tail := &tailByte{}
	// Do not chase a live writer beyond this inspection's initial file size.
	input := io.TeeReader(io.LimitReader(file, info.Size()-cached), tail)
	result, err := scanJSONLFrom(input, io.Discard, checkThread, true, cached == 0, cached > 0 && previous.Paginated)
	result.CachedPrefixBytes = cached
	certificate := sanitationCertificate{Stamp: sanitationStamp(info, path, threadID), Identity: identity, Size: info.Size(), EndsInNewline: tail.last == '\n', Paginated: result.paginated}
	return result, certificate, err
}
