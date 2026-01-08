package events

import (
    "crypto/rand"
    "encoding/hex"
    "sync"
    "time"
)

type Event struct {
    ID        string         `json:"id"`
    SessionID string         `json:"session_id"`
    Type      string         `json:"type"`
    Timestamp time.Time      `json:"timestamp"`
    Payload   map[string]any `json:"payload,omitempty"`
}

type Store struct {
    mu     sync.RWMutex
    bySess map[string][]Event
}

func NewStore() *Store {
    return &Store{bySess: make(map[string][]Event)}
}

func (s *Store) Append(sessionID, typ string, payload map[string]any) Event {
    evt := Event{
        ID:        randomID(),
        SessionID: sessionID,
        Type:      typ,
        Timestamp: time.Now().UTC(),
        Payload:   payload,
    }
    s.mu.Lock()
    s.bySess[sessionID] = append(s.bySess[sessionID], evt)
    s.mu.Unlock()
    return evt
}

func (s *Store) List(sessionID string) []Event {
    s.mu.RLock()
    defer s.mu.RUnlock()
    // return a shallow copy to avoid external mutation
    src := s.bySess[sessionID]
    out := make([]Event, len(src))
    copy(out, src)
    return out
}

func randomID() string {
    var b [16]byte
    _, _ = rand.Read(b[:])
    return hex.EncodeToString(b[:])
}

