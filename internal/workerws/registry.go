package workerws

import (
    "context"
    "encoding/json"
    "sync"
    ws "nhooyr.io/websocket"
)

// Registry keeps at most one worker connection per session.
type Registry struct {
    mu    sync.Mutex
    conns map[string]*ws.Conn
}

func NewRegistry() *Registry { return &Registry{conns: make(map[string]*ws.Conn)} }

// Replace sets the connection for a session and closes the previous one if present.
func (r *Registry) Replace(sessionID string, c *ws.Conn) (prevClosed bool) {
    r.mu.Lock()
    defer r.mu.Unlock()
    if old, ok := r.conns[sessionID]; ok && old != nil {
        _ = old.Close(ws.StatusNormalClosure, "replaced")
        prevClosed = true
    }
    r.conns[sessionID] = c
    return
}

func (r *Registry) Get(sessionID string) *ws.Conn {
    r.mu.Lock(); defer r.mu.Unlock()
    return r.conns[sessionID]
}

func (r *Registry) Remove(sessionID string) {
    r.mu.Lock(); defer r.mu.Unlock()
    delete(r.conns, sessionID)
}

// Send JSON helper with context.
func (r *Registry) SendJSON(ctx context.Context, sessionID string, v any) error {
    r.mu.Lock()
    c := r.conns[sessionID]
    r.mu.Unlock()
    if c == nil { return nil }
    return c.Write(ctx, ws.MessageText, mustJSON(v))
}

// local helper
func mustJSON(v any) []byte {
    b, _ := json.Marshal(v)
    return b
}
