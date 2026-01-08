package store

import (
	"testing"
	"time"
	"yuzu/agent/internal/types"
)

func TestCreateAndGetSession(t *testing.T) {
	st := New()
	s := &types.Session{ID: "abc123", CreatedAt: time.Now()}
	if err := st.CreateSession(s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	got := st.GetSession("abc123")
	if got == nil || got.ID != s.ID {
		t.Fatalf("expected session %q, got %#v", s.ID, got)
	}
}
