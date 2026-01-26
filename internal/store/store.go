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
    // worker state per session
    workerState map[string]WorkerState
}

func New() *Store {
    return &Store{
        sessions:   make(map[string]*types.Session),
        events:     make(map[string][]types.Event),
        botRunning: make(map[string]bool),
        workerState: make(map[string]WorkerState),
    }
}

// WorkerState captures worker capabilities and effective policy for a session.
type WorkerState struct {
    LocalStopCapable bool
    LocalStopEnabled bool
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
    if l := len(s.events[sessionID]); l > maxEvents {
        // Keep space for a single truncation warning so the total stays at maxEvents
        keep := maxEvents - 1
        if keep < 0 { keep = 0 }
        dropped := l - keep
        if dropped < 0 { dropped = 0 }
        if keep > 0 {
            s.events[sessionID] = append([]types.Event(nil), s.events[sessionID][l-keep:]...)
        } else {
            s.events[sessionID] = []types.Event{}
        }
        // Append warning event
        warn := types.Event{Type: "events_truncated", Ts: time.Now().UTC(), Payload: map[string]any{"session_id": sessionID, "dropped": dropped, "kept": keep}}
        s.events[sessionID] = append(s.events[sessionID], warn)
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

func (s *Store) ListSessionIDs() []string {
    s.mu.RLock()
    defer s.mu.RUnlock()
    out := make([]string, 0, len(s.sessions))
    for id := range s.sessions {
        out = append(out, id)
    }
    return out
}

// Worker state helpers
func (s *Store) SetLocalStopCapable(sessionID string, capable bool) {
    s.mu.Lock()
    st := s.workerState[sessionID]
    st.LocalStopCapable = capable
    s.workerState[sessionID] = st
    s.mu.Unlock()
}

func (s *Store) SetLocalStopEnabled(sessionID string, enabled bool) {
    s.mu.Lock()
    st := s.workerState[sessionID]
    st.LocalStopEnabled = enabled
    s.workerState[sessionID] = st
    s.mu.Unlock()
}

func (s *Store) GetWorkerState(sessionID string) WorkerState {
    s.mu.RLock(); defer s.mu.RUnlock()
    return s.workerState[sessionID]
}
