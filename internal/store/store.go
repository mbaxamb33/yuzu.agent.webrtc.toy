package store

import (
    "errors"
    "sync"
    "time"

    "yuzu/agent/internal/types"
)

var ErrSessionExists = errors.New("session already exists")

type Store struct {
    mu         sync.RWMutex
    sessions   map[string]*types.Session
    events     map[string][]types.Event
    botRunning map[string]bool
}

func New() *Store {
    return &Store{
        sessions:   make(map[string]*types.Session),
        events:     make(map[string][]types.Event),
        botRunning: make(map[string]bool),
    }
}

func (s *Store) CreateSession(sess *types.Session) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if _, ok := s.sessions[sess.ID]; ok {
        return ErrSessionExists
    }
    s.sessions[sess.ID] = sess
    s.events[sess.ID] = []types.Event{}
    return nil
}

func (s *Store) GetSession(id string) *types.Session {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.sessions[id]
}

func (s *Store) AppendEvent(sessionID, typ string, payload map[string]any) types.Event {
    evt := types.Event{Type: typ, Ts: time.Now().UTC(), Payload: payload}
    s.mu.Lock()
    defer s.mu.Unlock()
    s.events[sessionID] = append(s.events[sessionID], evt)
    // Cap total events per session to avoid unbounded growth
    const maxEvents = 200
    if len(s.events[sessionID]) > maxEvents {
        s.events[sessionID] = append([]types.Event(nil), s.events[sessionID][len(s.events[sessionID])-maxEvents:]...)
    }
    return evt
}

func (s *Store) ListEvents(sessionID string) []types.Event {
    s.mu.RLock()
    defer s.mu.RUnlock()
    src := s.events[sessionID]
    out := make([]types.Event, len(src))
    copy(out, src)
    return out
}

func (s *Store) SetBotRunning(sessionID string, running bool) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.botRunning[sessionID] = running
}

func (s *Store) IsBotRunning(sessionID string) bool {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.botRunning[sessionID]
}

func (s *Store) SetBotPID(sessionID string, pid int) {
    s.mu.Lock()
    if sess, ok := s.sessions[sessionID]; ok {
        sess.BotPID = pid
    }
    s.mu.Unlock()
}

func (s *Store) SetBotExit(sessionID string, code int, at time.Time) {
    s.mu.Lock()
    if sess, ok := s.sessions[sessionID]; ok {
        sess.BotLastExitCode = code
        sess.BotLastExitAt = &at
    }
    s.mu.Unlock()
}
