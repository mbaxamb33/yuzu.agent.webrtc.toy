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
    OnMessage func(sessionID string, msg Message)
    lastSeq  map[string]int64
}

func NewServer(cfg config.Config, st *store.Store, reg *Registry) *Server {
    return &Server{Cfg: cfg, Store: st, Reg: reg, lastSeq: make(map[string]int64)}
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
    s.lastSeq[sessionID] = 0

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
        // Handle hello -> capture capabilities and send policy
        if msg.Type == "worker_hello" {
            // parse local_stop_capable from payload
            if v, ok := msg.Payload["local_stop_capable"].(bool); ok {
                s.Store.SetLocalStopCapable(sessionID, v)
            }
            // Send policy if configured
            enabled := s.Cfg.Worker.LocalStopEnabled
            s.Store.SetLocalStopEnabled(sessionID, enabled)
            // Respond with policy message
            out := Message{Type: "policy", TsMs: time.Now().UnixMilli(), SessionID: sessionID, Payload: map[string]any{"local_stop_enabled": enabled}}
            ctxSend := r.Context()
            if err := s.Reg.SendJSON(ctxSend, sessionID, out); err != nil {
                s.Store.AppendEvent(sessionID, "worker_policy_send_error", map[string]any{"error": err.Error()})
            } else {
                s.Store.AppendEvent(sessionID, "worker_policy_sent", map[string]any{"local_stop_enabled": enabled})
            }
        }
        // Sequence gap detection
        prev := s.lastSeq[sessionID]
        if msg.Seq > prev+1 && prev != 0 {
            s.Store.AppendEvent(sessionID, "worker_seq_gap", map[string]any{"prev": prev, "now": msg.Seq, "gap": msg.Seq - prev})
        }
        if msg.Seq > prev { s.lastSeq[sessionID] = msg.Seq }
        if s.OnMessage != nil {
            s.OnMessage(sessionID, msg)
        }
    }
    _ = c.Close(ws.StatusNormalClosure, "done")
    s.Reg.Remove(sessionID)
    s.Store.AppendEvent(sessionID, "worker_disconnected", nil)
}
