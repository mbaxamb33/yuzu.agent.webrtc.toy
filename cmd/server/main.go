package main

import (
    "context"
    "errors"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "syscall"
    "time"

    "github.com/joho/godotenv"
    "yuzu/agent/internal/api"
    "yuzu/agent/internal/bot"
    "yuzu/agent/internal/config"
    "yuzu/agent/internal/daily"
    "yuzu/agent/internal/loop"
    "yuzu/agent/internal/store"
    "yuzu/agent/internal/workerws"
)

func main() {
	// Load .env file if present (ignored if missing)
	_ = godotenv.Load()

	cfg := config.Load()

	st := store.New()
	dailyClient := daily.NewClient(cfg.Daily.APIKey)

	runner := bot.NewLocalRunner(cfg.Bot.WorkerCmd, func(sessionID string, err error) {
		// On process exit, mark not running and append event.
		st.SetBotRunning(sessionID, false)
		code := exitCodeFromErr(err)
		st.SetBotExit(sessionID, code, time.Now().UTC())
		st.AppendEvent(sessionID, "bot_exit", map[string]any{
			"error": errString(err),
		})
	}, func(sessionID, stream, line string) {
		st.AppendEvent(sessionID, "bot_log", map[string]any{"stream": stream, "line": line})
	}, func(sessionID string, pid int) {
		st.SetBotPID(sessionID, pid)
	})

	h := api.NewHandlers(cfg, st, dailyClient, runner)
	mux := http.NewServeMux()
	mux.Handle("/", api.NewRouter(h))
	// WS worker route
	reg := workerws.NewRegistry()
	wss := workerws.NewServer(cfg, st, reg)
	// Dispatcher for Loop A floor control
	disp := loop.New(reg, st, 60)
	wss.OnMessage = disp.OnMessage
	mux.HandleFunc("/ws/worker", wss.HandleWorkerWS)

	addr := ":" + cfg.Server.Port
	srv := &http.Server{
		Addr:              addr,
		Handler:           logMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM
	sigc := make(chan os.Signal, 1)
    signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
    go func() {
        <-sigc
        log.Printf("shutdown signal received; stopping server...")
        // Stop running bots before draining HTTP
        for _, id := range st.ListSessionIDs() {
            if runner.IsRunning(id) {
                _ = runner.Stop(id)
            }
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = srv.Shutdown(ctx)
    }()

	log.Printf("server starting on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Println("server error:", err)
		os.Exit(1)
	}

    // Bots already stopped in signal handler above
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func exitCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
