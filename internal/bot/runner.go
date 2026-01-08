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

type LocalRunner struct {
    workerCmd string
    onExit    ExitCallback

    mu   sync.Mutex
    procs map[string]*proc
}

type proc struct {
    cmd    *exec.Cmd
    cancel context.CancelFunc
}

func NewLocalRunner(workerCmd string, onExit ExitCallback) *LocalRunner {
    return &LocalRunner{
        workerCmd: workerCmd,
        onExit:    onExit,
        procs:     make(map[string]*proc),
    }
}

func (r *LocalRunner) IsRunning(sessionID string) bool {
    r.mu.Lock(); defer r.mu.Unlock()
    _, ok := r.procs[sessionID]
    return ok
}

func (r *LocalRunner) Start(sessionID string, env map[string]string) error {
    r.mu.Lock()
    if _, exists := r.procs[sessionID]; exists {
        r.mu.Unlock()
        return errors.New("bot already running for session")
    }
    r.mu.Unlock()

    if strings.TrimSpace(r.workerCmd) == "" {
        return errors.New("worker command not configured")
    }

    parts := strings.Fields(r.workerCmd)
    name, args := parts[0], parts[1:]
    ctx, cancel := context.WithCancel(context.Background())
    cmd := exec.CommandContext(ctx, name, args...)

    // Start with current environment, then add ours
    cmd.Env = append(cmd.Env, envFromOS()...)
    cmd.Env = append(cmd.Env, envToList(env)...)

    stdout, _ := cmd.StdoutPipe()
    stderr, _ := cmd.StderrPipe()

    if err := cmd.Start(); err != nil {
        cancel()
        return err
    }

    r.mu.Lock()
    r.procs[sessionID] = &proc{cmd: cmd, cancel: cancel}
    r.mu.Unlock()

    // Log stdout/stderr
    go stream("bot["+sessionID+"] stdout", stdout)
    go stream("bot["+sessionID+"] stderr", stderr)

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

func stream(prefix string, rdr interface{ Read([]byte) (int, error) }) {
    scanner := bufio.NewScanner(rdr)
    for scanner.Scan() {
        log.Printf("%s: %s", prefix, scanner.Text())
    }
}
