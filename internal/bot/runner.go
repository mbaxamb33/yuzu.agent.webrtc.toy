package bot

import (
	"bufio"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Runner is the interface used by handlers for starting/stopping bot workers.
type Runner interface {
	Start(sessionID string, env map[string]string) error
	Stop(sessionID string) error
	IsRunning(sessionID string) bool
}

// ExitCallback is invoked when a session's worker process exits (naturally or killed).
type ExitCallback func(sessionID string, err error)
type LogCallback func(sessionID string, stream string, line string)
type StartCallback func(sessionID string, pid int)

type LocalRunner struct {
	workerCmd string
	onExit    ExitCallback
	onLog     LogCallback
	onStart   StartCallback

	mu    sync.Mutex
	procs map[string]*proc
}

type proc struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

func NewLocalRunner(workerCmd string, onExit ExitCallback, onLog LogCallback, onStart StartCallback) *LocalRunner {
	return &LocalRunner{
		workerCmd: workerCmd,
		onExit:    onExit,
		onLog:     onLog,
		onStart:   onStart,
		procs:     make(map[string]*proc),
	}
}

func (r *LocalRunner) IsRunning(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.procs[sessionID]
	return ok
}

func (r *LocalRunner) Start(sessionID string, env map[string]string) error {
	if strings.TrimSpace(r.workerCmd) == "" {
		return errors.New("worker command not configured")
	}

	parts := strings.Fields(r.workerCmd)
	name, args := parts[0], parts[1:]
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, name, args...)

	// Reserve slot to prevent TOCTOU duplicate starts
	r.mu.Lock()
	if _, exists := r.procs[sessionID]; exists {
		r.mu.Unlock()
		cancel()
		return errors.New("bot already running for session")
	}
	r.procs[sessionID] = &proc{cmd: nil, cancel: cancel}
	r.mu.Unlock()

	// Start with current environment, then add ours
	cmd.Env = append(cmd.Env, envFromOS()...)
	cmd.Env = append(cmd.Env, envToList(env)...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.mu.Lock()
		delete(r.procs, sessionID)
		r.mu.Unlock()
		cancel()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		r.mu.Lock()
		delete(r.procs, sessionID)
		r.mu.Unlock()
		cancel()
		return err
	}

	if err := cmd.Start(); err != nil {
		r.mu.Lock()
		delete(r.procs, sessionID)
		r.mu.Unlock()
		cancel()
		return err
	}

	r.mu.Lock()
	r.procs[sessionID] = &proc{cmd: cmd, cancel: cancel}
	r.mu.Unlock()

	if r.onStart != nil && cmd.Process != nil {
		r.onStart(sessionID, cmd.Process.Pid)
	}

	// Log stdout/stderr
	go r.stream(sessionID, "stdout", stdout)
	go r.stream(sessionID, "stderr", stderr)

	// Wait and cleanup
	go func() {
		err := cmd.Wait()
		r.mu.Lock()
		delete(r.procs, sessionID)
		r.mu.Unlock()
		if r.onExit != nil {
			r.onExit(sessionID, err)
		}
	}()

	return nil
}

func (r *LocalRunner) Stop(sessionID string) error {
	r.mu.Lock()
	p, ok := r.procs[sessionID]
	r.mu.Unlock()
	if !ok {
		return errors.New("bot not running for session")
	}
	// request context cancel, then force kill after grace
	p.cancel()
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		return nil
	}
}

func envToList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func envFromOS() []string {
	// Clone to avoid accidental modification of the returned backing array
	base := os.Environ()
	out := make([]string, len(base))
	copy(out, base)
	return out
}

func (r *LocalRunner) stream(sessionID, stream string, rdr interface{ Read([]byte) (int, error) }) {
	scanner := bufio.NewScanner(rdr)
	for scanner.Scan() {
		line := scanner.Text()
		log.Printf("bot[%s] %s: %s", sessionID, stream, line)
		if r.onLog != nil {
			r.onLog(sessionID, stream, line)
		}
	}
}
