package api

import (
    "encoding/json"
    "net/http"
    "time"

    "github.com/google/uuid"
    "yuzu/agent/internal/bot"
    "yuzu/agent/internal/config"
    "yuzu/agent/internal/daily"
    "yuzu/agent/internal/store"
    "yuzu/agent/internal/types"
)

type Handlers struct {
    cfg    config.Config
    store  *store.Store
    daily  daily.Client
    runner bot.Runner
}

func NewHandlers(cfg config.Config, st *store.Store, d daily.Client, r bot.Runner) *Handlers {
    return &Handlers{cfg: cfg, store: st, daily: d, runner: r}
}

func (h *Handlers) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
    if h.cfg.Daily.APIKey == "" || h.cfg.Daily.Domain == "" {
        http.Error(w, "missing Daily configuration", http.StatusBadRequest)
        return
    }
    // Generate session ID
    id := uuid.New().String()
    roomName := h.cfg.Daily.RoomPrefix + id
    roomURL := "https://" + h.cfg.Daily.Domain + "/" + roomName

    // Create room in Daily
    if err := h.daily.CreateRoom(roomName, h.cfg.Daily.RoomPrivacy); err != nil {
        http.Error(w, err.Error(), http.StatusBadGateway)
        return
    }

    // Create meeting token
    exp := time.Now().Add(time.Duration(h.cfg.Daily.BotTokenExpMin) * time.Minute).Unix()
    token, err := h.daily.CreateMeetingToken(roomName, h.cfg.Daily.BotName, exp)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadGateway)
        return
    }

    sess := &types.Session{
        ID:        id,
        RoomName:  roomName,
        RoomURL:   roomURL,
        BotToken:  token,
        CreatedAt: time.Now().UTC(),
        Status:    "created",
    }
    _ = h.store.CreateSession(sess)
    h.store.AppendEvent(id, "session_created", map[string]any{"room_name": roomName})

    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    _ = json.NewEncoder(w).Encode(map[string]any{
        "session_id": id,
        "room_name":  roomName,
        "room_url":   roomURL,
        "bot_token":  token,
    })
}

func (h *Handlers) HandleStartSession(w http.ResponseWriter, r *http.Request, id string) {
    sess := h.store.GetSession(id)
    if sess == nil {
        http.NotFound(w, r)
        return
    }
    running := h.store.IsBotRunning(id)
    if running {
        h.store.AppendEvent(id, "bot_start_requested", map[string]any{"noop": true})
        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "running": true})
        return
    }
    h.store.AppendEvent(id, "bot_start_requested", nil)

    env := map[string]string{
        "DAILY_ROOM_URL":             sess.RoomURL,
        "DAILY_TOKEN":                 sess.BotToken,
        "ELEVENLABS_API_KEY":          h.cfg.Eleven.APIKey,
        "ELEVENLABS_VOICE_ID":         h.cfg.Eleven.VoiceID,
        "ELEVENLABS_CANNED_PHRASE":    h.cfg.Eleven.CannedPhrase,
        "BOT_STAY_CONNECTED_SECONDS":  h.cfg.Bot.StayConnectedSeconds,
    }
    if err := h.runner.Start(id, env); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    h.store.SetBotRunning(id, true)
    h.store.AppendEvent(id, "bot_started", nil)

    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "running": true})
}

func (h *Handlers) HandleEndSession(w http.ResponseWriter, r *http.Request, id string) {
    sess := h.store.GetSession(id)
    if sess == nil {
        http.NotFound(w, r)
        return
    }
    running := h.runner.IsRunning(id)
    if !running {
        h.store.AppendEvent(id, "bot_stop_requested", map[string]any{"noop": true})
        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "running": false})
        return
    }
    h.store.AppendEvent(id, "bot_stop_requested", nil)
    if running {
        _ = h.runner.Stop(id)
        h.store.SetBotRunning(id, false)
    }
    h.store.AppendEvent(id, "bot_stopped", nil)

    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "running": false})
}

func (h *Handlers) HandleListEvents(w http.ResponseWriter, r *http.Request, id string) {
    sess := h.store.GetSession(id)
    if sess == nil {
        http.NotFound(w, r)
        return
    }
    events := h.store.ListEvents(id)
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]any{
        "session_id": id,
        "events":     events,
    })
}
