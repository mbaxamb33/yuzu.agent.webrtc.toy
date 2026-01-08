package main

import (
    "log"
    "net/http"
    "os"
    "time"

    "yuzu/agent/internal/api"
    "yuzu/agent/internal/bot"
    "yuzu/agent/internal/config"
    "yuzu/agent/internal/daily"
    "yuzu/agent/internal/store"
)

func main() {
    cfg := config.Load()

    st := store.New()
    dailyClient := daily.NewClient(cfg.Daily.APIKey)

    runner := bot.NewLocalRunner(cfg.Bot.WorkerCmd, func(sessionID string, err error) {
        // On process exit, mark not running and append event.
        st.SetBotRunning(sessionID, false)
        st.AppendEvent(sessionID, "bot_exit", map[string]any{
            "error": errString(err),
        })
    })

    h := api.NewHandlers(cfg, st, dailyClient, runner)
    mux := api.NewRouter(h)

    addr := ":" + cfg.Server.Port
    srv := &http.Server{
        Addr:              addr,
        Handler:           logMiddleware(mux),
        ReadHeaderTimeout: 5 * time.Second,
    }

    log.Printf("server starting on %s", addr)
    if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        log.Println("server error:", err)
        os.Exit(1)
    }
}

func errString(err error) string {
    if err == nil {
        return ""
    }
    return err.Error()
}

func logMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        next.ServeHTTP(w, r)
        log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
    })
}
