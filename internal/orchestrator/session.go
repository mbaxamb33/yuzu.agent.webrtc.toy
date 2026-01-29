package orchestrator

import "context"

// session.go groups session-related helpers. The sessionState type lives in server.go.

// attachLLM stores cancel and flags on the session state safely.
func (s *Server) attachLLM(sessionID string, cancel context.CancelFunc) {
    s.mu.Lock()
    if st := s.sess[sessionID]; st != nil {
        st.llmCancel = cancel
        st.llmActive = true
    }
    s.mu.Unlock()
}

// detachLLM clears LLM flags after stream finishes.
func (s *Server) detachLLM(sessionID string) {
    s.mu.Lock()
    if st := s.sess[sessionID]; st != nil {
        st.llmActive = false
        st.llmCancel = nil
    }
    s.mu.Unlock()
}

