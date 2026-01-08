package api

import (
    "net/http"
    "net/http/httptest"
    "testing"

    "yuzu/agent/internal/bot"
    "yuzu/agent/internal/config"
    "yuzu/agent/internal/daily"
    "yuzu/agent/internal/store"
)

type mockDaily struct{}
func (m *mockDaily) CreateRoom(name, privacy string) error { return nil }
func (m *mockDaily) CreateMeetingToken(roomName, userName string, exp int64) (string, error) { return "tok", nil }

type mockRunner struct{}
func (m *mockRunner) Start(sessionID string, env map[string]string) error { return nil }
func (m *mockRunner) Stop(sessionID string) error { return nil }
func (m *mockRunner) IsRunning(sessionID string) bool { return false }

func TestStartEndUnknownSession404(t *testing.T) {
    cfg := config.Load()
    st := store.New()
    var d daily.Client = &mockDaily{}
    var r bot.Runner = &mockRunner{}
    h := NewHandlers(cfg, st, d, r)
    srv := httptest.NewServer(NewRouter(h))
    defer srv.Close()

    // POST /sessions/unknown/start
    resp, err := http.Post(srv.URL+"/sessions/unknown/start", "application/json", nil)
    if err != nil { t.Fatalf("request: %v", err) }
    if resp.StatusCode != http.StatusNotFound {
        t.Fatalf("expected 404, got %d", resp.StatusCode)
    }

    // POST /sessions/unknown/end
    resp, err = http.Post(srv.URL+"/sessions/unknown/end", "application/json", nil)
    if err != nil { t.Fatalf("request: %v", err) }
    if resp.StatusCode != http.StatusNotFound {
        t.Fatalf("expected 404, got %d", resp.StatusCode)
    }
}

