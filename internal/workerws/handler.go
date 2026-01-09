package workerws

import (
    "encoding/json"
    "log"
    "net/http"
    "strings"
    "time"

    "yuzu/agent/internal/auth"
    "yuzu/agent/internal/config"
    "yuzu/agent/internal/store"

    ws "nhooyr.io/websocket"
)

type Message struct {
    Type        string         `json:"type"`
    TsMs        int64          `json:"ts_ms"`
    SessionID   string         `json:"session_id"`
    Seq         int64          `json:"seq"`
    CommandID   string         `json:"command_id,omitempty"`
    UtteranceID string         `json:"utterance_id,omitempty"`
    Payload     map[string]any `json:"payload,omitempty"`
}

type Server struct {
    Cfg      config.Config
    Store    *store.Store
    Reg      *Registry
}

func NewServer(cfg config.Config, st *store.Store, reg *Registry) *Server {
    return &Server{Cfg: cfg, Store: st, Reg: reg}
}

func (s *Server) HandleWorkerWS(w http.ResponseWriter, r *http.Request) {
    q := r.URL.Query()
    sessionID := q.Get("session_id")
    if sessionID == "" {
        http.Error(w, "missing session_id", http.StatusBadRequest)
        return
    }
    if s.Store.GetSession(sessionID) == nil {
        http.Error(w, "unknown session", http.StatusNotFound)
        return
    }
    // Auth header
    authz := r.Header.Get("Authorization")
    if !strings.HasPrefix(authz, "Bearer ") {
        http.Error(w, "missing bearer token", http.StatusUnauthorized)
        return
    }
    token := strings.TrimPrefix(authz, "Bearer ")
    if s.Cfg.Worker.TokenSecret == "" {
        http.Error(w, "worker auth not configured", http.StatusUnauthorized)
        return
    }
    if _, _, err := auth.ValidateWorkerToken(s.Cfg.Worker.TokenSecret, token, sessionID, time.Now(), s.Cfg.Worker.TokenSkewSecs); err != nil {
        http.Error(w, "invalid token", http.StatusUnauthorized)
        return
    }

    c, err := ws.Accept(w, r, nil)
    if err != nil {
        log.Printf("ws accept: %v", err)
        return
    }
    replaced := s.Reg.Replace(sessionID, c)
    if replaced {
        s.Store.AppendEvent(sessionID, "worker_replaced", nil)
    }
    s.Store.AppendEvent(sessionID, "worker_connected", nil)

    ctx := r.Context()
    for {
        typ, data, err := c.Read(ctx)
        if err != nil {
            break
        }
        if typ != ws.MessageText && typ != ws.MessageBinary {
            continue
        }
        var msg Message
        if err := json.Unmarshal(data, &msg); err != nil {
            s.Store.AppendEvent(sessionID, "worker_msg_invalid", map[string]any{"error": err.Error()})
            continue
        }
        payload := msg.Payload
        if payload == nil { payload = map[string]any{} }
        payload["ts_ms"] = msg.TsMs
        payload["seq"] = msg.Seq
        if msg.CommandID != "" { payload["command_id"] = msg.CommandID }
        if msg.UtteranceID != "" { payload["utterance_id"] = msg.UtteranceID }
        s.Store.AppendEvent(sessionID, msg.Type, payload)
    }
    _ = c.Close(ws.StatusNormalClosure, "done")
    s.Reg.Remove(sessionID)
    s.Store.AppendEvent(sessionID, "worker_disconnected", nil)
}
