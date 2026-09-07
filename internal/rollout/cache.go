package rollout

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type sanitationCertificate struct {
	Stamp         string `json:"stamp"`
	Identity      string `json:"identity"`
	Size          int64  `json:"size"`
	Digest        string `json:"sha256"`
	Paginated     bool   `json:"paginated"`
	EndsInNewline bool   `json:"ends_in_newline"`
}

// ctime detects edits that restore size and mtime. The version invalidates
// certificates when sanitation rules change; identity binds prefix hashes to
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
	if err != nil || json.Unmarshal(data, &certificate) != nil || len(certificate.Stamp) != 64 || len(certificate.Identity) != 64 || len(certificate.Digest) != 64 || certificate.Size < 0 {
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

type digestWithTail struct {
	hash.Hash
	last byte
}

func (digest *digestWithTail) Write(data []byte) (int, error) {
	if len(data) > 0 {
		digest.last = data[len(data)-1]
	}
	return digest.Hash.Write(data)
}

func inspectSanitation(file *os.File, info os.FileInfo, path, threadID string, previous sanitationCertificate) (Result, sanitationCertificate, error) {
	digest := &digestWithTail{Hash: sha256.New()}
	identity := sanitationIdentity(info, path, threadID)
	verified := int64(0)
	if previous.Identity == identity && identity != "" && previous.EndsInNewline && previous.Size > 0 && previous.Size < info.Size() {
		// Do not assume append-only history: verify every previously certified byte
		// with a digest before skipping its more expensive JSON decoding.
		if _, err := io.CopyN(digest, file, previous.Size); err == nil && fmt.Sprintf("%x", digest.Sum(nil)) == previous.Digest {
			verified = previous.Size
		} else {
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return Result{}, sanitationCertificate{}, err
			}
			digest = &digestWithTail{Hash: sha256.New()}
		}
	}
	checkThread := threadID
	if verified > 0 {
		checkThread = ""
	} // The verified prefix already contains session_meta.
	result, err := scanJSONLFrom(io.TeeReader(file, digest), io.Discard, checkThread, true, verified == 0, verified > 0 && previous.Paginated)
	result.VerifiedPrefixBytes = verified
	certificate := sanitationCertificate{Stamp: sanitationStamp(info, path, threadID), Identity: identity, Size: info.Size(), Digest: fmt.Sprintf("%x", digest.Sum(nil)), EndsInNewline: digest.last == '\n', Paginated: result.paginated}
	return result, certificate, err
}
