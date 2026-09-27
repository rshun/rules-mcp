package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type Runner interface {
	Run(context.Context, string, []string, []byte) ([]byte, error)
}
type ExecRunner struct{ Directory string }

func (r ExecRunner) Run(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Directory
	// Inherit SSH agent access, but never allow Git repository/config injection.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes -oStrictHostKeyChecking=yes", "LC_ALL=C")
	cmd.Stdin = bytes.NewReader(input)
	var out bytes.Buffer
	cmd.Stdout = &out
	// Remote errors may contain credentials, URLs or arbitrary server output.
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("command timed out or was cancelled")
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("command failed (exit %d); inspect credentials and remote state locally", exit.ExitCode())
		}
		return nil, fmt.Errorf("cannot start required executable")
	}
	return out.Bytes(), nil
}

func (a *App) git(ctx context.Context, args ...string) (string, error) {
	// Never execute repository hooks or signing commands from MCP requests.
	prefix := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgSign=false", "-c", "merge.autoStash=false"}
	b, err := a.runner.Run(ctx, "git", append(prefix, args...), nil)
	return strings.TrimSpace(string(b)), err
}
