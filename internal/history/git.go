package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ErrNoGit is returned when git isn't on PATH: history is off.
var ErrNoGit = errors.New("git not found: state history is off")

// Available reports whether git is on PATH.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// Git runs git for arcus: never as root, never on the terminal, and never
// for longer than its timeout.
type Git struct {
	// Euid is the effective user id; git is refused at 0, so the root side
	// never runs a user's git configuration or helpers.
	Euid int
	// Files are passed to every child after stdin, stdout and stderr: the
	// user lock, so it stays held while a child of a killed arcus still
	// runs.
	Files []*os.File
	// Timeout is per call; zero means 30 seconds.
	Timeout time.Duration
	// WaitDelay bounds how long a call waits for its output after git
	// exited; zero means two seconds.
	WaitDelay time.Duration
}

// exitError is a git call that ran and exited non-zero.
type exitError struct {
	args   []string
	code   int
	stderr string
}

func (e *exitError) Error() string {
	msg := fmt.Sprintf("git %s: exit status %d", e.args[0], e.code)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

// exitCode returns the exit status of a git call that ran, or -1.
func exitCode(err error) int {
	if e, ok := errors.AsType[*exitError](err); ok {
		return e.code
	}
	return -1
}

// run runs git in dir with env (added to the environment without any
// GIT_* variable) and stdin, and returns its standard output.
func (g Git) run(dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	if g.Euid == 0 {
		return nil, errors.New("arcus doesn't run git as root")
	}
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	} // nil is /dev/null
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.ExtraFiles = g.Files
	// Its own process group: a terminal Ctrl-C doesn't reach it, and the
	// whole group, gc's children included, can be killed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = g.WaitDelay
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = 2 * time.Second
	}
	err := cmd.Run()
	if cmd.Process != nil {
		// Whatever Wait returned, nothing git started outlives the call: a
		// descendant still holding the output, or the user lock, goes too.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, fmt.Errorf("git %s: timed out after %v", args[0], timeout)
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		// ErrWaitDelay: git succeeded; a descendant held the output open.
		return stdout.Bytes(), nil
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.Exited() {
		first, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		return stdout.Bytes(), &exitError{args: args, code: exit.ExitCode(), stderr: first}
	}
	return nil, fmt.Errorf("git %s: %w", args[0], err)
}
