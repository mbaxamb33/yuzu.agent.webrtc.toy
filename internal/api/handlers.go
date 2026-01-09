package api

import (
    "encoding/json"
    "log"
    "net/http"
    "time"

    "github.com/google/uuid"
    "yuzu/agent/internal/bot"
    "yuzu/agent/internal/config"
    "yuzu/agent/internal/daily"
    "yuzu/agent/internal/auth"
    "yuzu/agent/internal/store"
    "yuzu/agent/internal/types"
    "yuzu/agent/internal/workerws"
)

type Handlers struct {
    cfg    config.Config
    store  *store.Store
    daily  daily.Client
    runner bot.Runner
    onWorkerMsg func(sessionID string, msg workerws.Message)
}

func NewHandlers(cfg config.Config, st *store.Store, d daily.Client, r bot.Runner) *Handlers {
    return &Handlers{cfg: cfg, store: st, daily: d, runner: r}
}

func (h *Handlers) SetOnWorkerMessage(fn func(sessionID string, msg workerws.Message)) { h.onWorkerMsg = fn }

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
	if err := h.store.CreateSession(sess); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.store.AppendEvent(id, "session_created", map[string]any{"room_name": roomName})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"room_name":  roomName,
		"room_url":   roomURL,
		"bot_token":  token,
	}); err != nil {
		log.Printf("encode error: %v", err)
	}
}

func (h *Handlers) HandleStartSession(w http.ResponseWriter, r *http.Request, id string) {
	sess := h.store.GetSession(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	running := h.runner.IsRunning(id)
	if running {
		h.store.AppendEvent(id, "bot_start_requested", map[string]any{"noop": true})
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "running": true}); err != nil {
			log.Printf("encode error: %v", err)
		}
		return
	}
	h.store.AppendEvent(id, "bot_start_requested", nil)

	env := map[string]string{
		"DAILY_ROOM_URL":             sess.RoomURL,
		"DAILY_TOKEN":                sess.BotToken,
		"ELEVENLABS_API_KEY":         h.cfg.Eleven.APIKey,
		"ELEVENLABS_VOICE_ID":        h.cfg.Eleven.VoiceID,
		"ELEVENLABS_CANNED_PHRASE":   h.cfg.Eleven.CannedPhrase,
		"BOT_STAY_CONNECTED_SECONDS": h.cfg.Bot.StayConnectedSeconds,
	}
	if err := h.runner.Start(id, env); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.store.SetBotRunning(id, true)
	h.store.AppendEvent(id, "bot_started", nil)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "running": true}); err != nil {
		log.Printf("encode error: %v", err)
	}
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
		if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "running": false}); err != nil {
			log.Printf("encode error: %v", err)
		}
		return
	}
	h.store.AppendEvent(id, "bot_stop_requested", nil)
	if running {
		_ = h.runner.Stop(id)
		h.store.SetBotRunning(id, false)
	}
	h.store.AppendEvent(id, "bot_stopped", nil)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "running": false}); err != nil {
		log.Printf("encode error: %v", err)
	}
}

func (h *Handlers) HandleListEvents(w http.ResponseWriter, r *http.Request, id string) {
    sess := h.store.GetSession(id)
    if sess == nil {
        http.NotFound(w, r)
        return
    }
    events := h.store.ListEvents(id)
    w.Header().Set("Content-Type", "application/json")
    if err := json.NewEncoder(w).Encode(map[string]any{
        "session_id": id,
        "events":     events,
    }); err != nil { log.Printf("encode error: %v", err) }
}

// Dev-only: mint worker token
func (h *Handlers) HandleMintWorkerToken(w http.ResponseWriter, r *http.Request, id string) {
    if !h.devAuthorized(r) {
        http.Error(w, "forbidden", http.StatusForbidden)
        return
    }
    if h.cfg.Worker.TokenSecret == "" {
        http.Error(w, "worker token not configured", http.StatusBadRequest)
        return
    }
    if h.store.GetSession(id) == nil {
        http.Error(w, "unknown session", http.StatusNotFound)
        return
    }
    exp := time.Now().Add(time.Duration(h.cfg.Worker.TokenTTLSecs) * time.Second).Unix()
    tok, err := auth.GenerateWorkerToken(h.cfg.Worker.TokenSecret, id, exp)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    if err := json.NewEncoder(w).Encode(map[string]any{"token": tok, "exp_unix": exp}); err != nil { log.Printf("encode error: %v", err) }
}

// Dev-only: WS URL + token in one shot
func (h *Handlers) HandleMintWSCreds(w http.ResponseWriter, r *http.Request, id string) {
    if !h.devAuthorized(r) {
        http.Error(w, "forbidden", http.StatusForbidden)
        return
    }
    if h.cfg.Worker.TokenSecret == "" {
        http.Error(w, "worker token not configured", http.StatusBadRequest)
        return
    }
    if h.store.GetSession(id) == nil {
        http.Error(w, "unknown session", http.StatusNotFound)
        return
    }
    exp := time.Now().Add(time.Duration(h.cfg.Worker.TokenTTLSecs) * time.Second).Unix()
    tok, err := auth.GenerateWorkerToken(h.cfg.Worker.TokenSecret, id, exp)
    if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
    wsURL := "ws://" + r.Host + "/ws/worker?session_id=" + id
    w.Header().Set("Content-Type", "application/json")
    if err := json.NewEncoder(w).Encode(map[string]any{"ws_url": wsURL, "worker_token": tok, "exp_unix": exp}); err != nil { log.Printf("encode error: %v", err) }
}

// Dev-only: inject VAD start/end
func (h *Handlers) HandleDebugVAD(w http.ResponseWriter, r *http.Request, id string, typ string) {
    if !h.devAuthorized(r) {
        http.Error(w, "forbidden", http.StatusForbidden)
        return
    }
    if h.store.GetSession(id) == nil {
        http.Error(w, "unknown session", http.StatusNotFound)
        return
    }
    if h.onWorkerMsg == nil {
        http.Error(w, "dispatcher not ready", http.StatusServiceUnavailable)
        return
    }
    msg := workerws.Message{Type: typ, TsMs: time.Now().UnixMilli(), SessionID: id, Seq: 0, Payload: map[string]any{"source":"debug"}}
    h.onWorkerMsg(id, msg)
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (h *Handlers) devAuthorized(r *http.Request) bool {
    if h.cfg.Dev.Mode {
        return true
    }
    if h.cfg.Dev.Key == "" {
        return false
    }
    return r.Header.Get("X-Dev-Key") == h.cfg.Dev.Key
}
