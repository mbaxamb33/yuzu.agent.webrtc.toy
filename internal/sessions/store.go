package sessions

import (
    "crypto/rand"
    "encoding/hex"
    "sync"
    "time"
)

type Session struct {
    ID        string       `json:"id"`
    CreatedAt time.Time    `json:"created_at"`
    Room      RoomMetadata `json:"room"`
}

type RoomMetadata struct {
    Name    string `json:"name"`
    JoinURL string `json:"join_url"`
}

type Store struct {
    mu       sync.RWMutex
    sessions map[string]*Session
}

func NewStore() *Store {
    return &Store{sessions: make(map[string]*Session)}
}

func (s *Store) Create() *Session {
    id := randomID()
    return &Session{
        ID:        id,
        CreatedAt: time.Now().UTC(),
    }
}

func (s *Store) Put(sess *Session) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.sessions[sess.ID] = sess
}

func (s *Store) Get(id string) *Session {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.sessions[id]
}

func randomID() string {
    var b [16]byte
    _, _ = rand.Read(b[:])
    return hex.EncodeToString(b[:])
}

