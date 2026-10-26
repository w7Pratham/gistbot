package opencode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// modelsTimeout bounds how long listing installed models may take.
const modelsTimeout = 15 * time.Second

// ListModels returns the model identifiers reported by `opencode models`, one
// per non-empty trimmed line. The command is executed directly with a fixed
// argv; no shell is involved.
func ListModels(ctx context.Context, bin string) ([]string, error) {
	if ctx == nil {
		return nil, errors.New("opencode models: context is nil")
	}
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return nil, errors.New("opencode models: binary path is empty")
	}

	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	// The argv is fixed. The binary path is configuration, not user input, and
	// is never passed through a shell.
	cmd := exec.CommandContext(ctx, bin, "models")
	output, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("opencode models: %w", ctxErr)
		}
		return nil, fmt.Errorf("opencode models: %w", err)
	}

	lines := strings.Split(string(output), "\n")
	models := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		models = append(models, line)
	}
	return models, nil
}
