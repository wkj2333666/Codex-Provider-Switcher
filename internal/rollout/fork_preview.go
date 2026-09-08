package rollout

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"time"
)

//go:embed fork_preview.py
var forkPreviewScript string

// RepairForkPreview repairs native paginated forks omitted by thread/list's
// empty-preview filter. Only derived SQLite metadata is changed; rollout byte
// offsets and shared history remain untouched. Python's SQLite implementation
// avoids adding a second SQLite implementation to the CGO-free proxy binary.
func RepairForkPreview(ctx context.Context, home, threadID string) (string, error) {
	if !filepath.IsAbs(home) || !safeThreadID(threadID) {
		return "", errors.New("invalid fork preview target")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-I", "-c", forkPreviewScript, home, threadID)
	output, err := command.Output()
	if err != nil {
		return "", errors.New("fork preview metadata repair unavailable")
	}
	var preview string
	if len(output) > 64*1024 || json.Unmarshal(output, &preview) != nil {
		return "", errors.New("invalid fork preview repair result")
	}
	return preview, nil
}
