package orchestrator

import (
	"context"
	"testing"
	"time"
)

func TestVADThresholds(t *testing.T) {
	s := NewServer()
	st := &sessionState{
		minStart: 2,
		hangover: 3,
		minRMS:   1000.0,
	}

	// Below threshold - no speech start
	for i := 0; i < 5; i++ {
		s.handleFeaturePrimary(st, 500.0, time.Now(), "test", nil)
	}
	if st.speaking {
		t.Error("should not be speaking with RMS below threshold")
	}
	if st.consecSpeech != 0 {
		t.Errorf("consecSpeech should be 0, got %d", st.consecSpeech)
	}
}

func TestVADSpeechStart(t *testing.T) {
	s := NewServer()
	st := &sessionState{
		minStart: 3, // Use 3 so we can test incrementing without triggering send
		hangover: 3,
		minRMS:   1000.0,
	}

	// First frame above threshold
	s.handleFeaturePrimary(st, 1500.0, time.Now(), "test", nil)
	if st.speaking {
		t.Error("should not be speaking after just 1 frame")
	}
	if st.consecSpeech != 1 {
		t.Errorf("consecSpeech should be 1, got %d", st.consecSpeech)
	}

	// Second frame above threshold - still not speaking
	s.handleFeaturePrimary(st, 1500.0, time.Now(), "test", nil)
	if st.speaking {
		t.Error("should not be speaking after 2 frames (minStart=3)")
	}
	if st.consecSpeech != 2 {
		t.Errorf("consecSpeech should be 2, got %d", st.consecSpeech)
	}
}

func TestVADSpeechEnd(t *testing.T) {
	s := NewServer()
	st := &sessionState{
		minStart: 2,
		hangover: 3,
		minRMS:   1000.0,
		speaking: true,
	}

	// Frames below threshold
	for i := 0; i < 2; i++ {
		s.handleFeaturePrimary(st, 500.0, time.Now(), "test", nil)
	}
	if !st.speaking {
		t.Error("should still be speaking (hangover not reached)")
	}
	if st.nonSpeech != 2 {
		t.Errorf("nonSpeech should be 2, got %d", st.nonSpeech)
	}

	// Third frame below threshold - should end speech
	s.handleFeaturePrimary(st, 500.0, time.Now(), "test", nil)
	if st.speaking {
		t.Error("should stop speaking after hangover frames")
	}
}

func TestVADGuardBlock(t *testing.T) {
	s := NewServer()
	now := time.Now()
	st := &sessionState{
		minStart:   2,
		hangover:   3,
		minRMS:     1000.0,
		guardUntil: now.Add(500 * time.Millisecond),
	}

	// During guard window, high RMS should be blocked
	triggered := s.handleFeaturePrimary(st, 1500.0, now, "test", nil)
	if triggered {
		t.Error("should not trigger barge-in during guard window")
	}
	if st.consecSpeech != 0 {
		t.Error("should not count speech during guard window")
	}
}

func TestVADConsecSpeechReset(t *testing.T) {
	s := NewServer()
	st := &sessionState{
		minStart: 3,
		hangover: 3,
		minRMS:   1000.0,
	}

	// Two frames above threshold
	s.handleFeaturePrimary(st, 1500.0, time.Now(), "test", nil)
	s.handleFeaturePrimary(st, 1500.0, time.Now(), "test", nil)
	if st.consecSpeech != 2 {
		t.Errorf("consecSpeech should be 2, got %d", st.consecSpeech)
	}

	// One frame below threshold - should reset
	s.handleFeaturePrimary(st, 500.0, time.Now(), "test", nil)
	if st.consecSpeech != 0 {
		t.Errorf("consecSpeech should reset to 0, got %d", st.consecSpeech)
	}
}

func TestCancelLLM(t *testing.T) {
	s := NewServer()
	cancelled := false
	st := &sessionState{
		llmActive: true,
		llmCancel: func() { cancelled = true },
	}

	s.cancelLLM(st)

	if !cancelled {
		t.Error("cancel function should have been called")
	}
	if st.llmActive {
		t.Error("llmActive should be false after cancel")
	}
	if st.llmCancel != nil {
		t.Error("llmCancel should be nil after cancel")
	}
}

func TestCancelLLMNoOp(t *testing.T) {
	s := NewServer()
	st := &sessionState{
		llmActive: false,
		llmCancel: nil,
	}

	// Should not panic
	s.cancelLLM(st)
}

func TestAttachDetachLLM(t *testing.T) {
	s := NewServer()
	sid := "test-session"
	s.sess[sid] = &sessionState{id: sid}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.attachLLM(sid, cancel)

	st := s.sess[sid]
	if !st.llmActive {
		t.Error("llmActive should be true after attach")
	}
	if st.llmCancel == nil {
		t.Error("llmCancel should be set after attach")
	}

	s.detachLLM(sid)

	if st.llmActive {
		t.Error("llmActive should be false after detach")
	}
	if st.llmCancel != nil {
		t.Error("llmCancel should be nil after detach")
	}
}

func TestArmBargeIn(t *testing.T) {
	s := NewServer()
	st := &sessionState{}

	before := time.Now()
	s.armBargeIn(st, 500, 1200)
	after := time.Now()

	if st.minRMS != 1200.0 {
		t.Errorf("minRMS should be 1200, got %f", st.minRMS)
	}
	if st.armedAt.Before(before) || st.armedAt.After(after) {
		t.Error("armedAt should be set to current time")
	}
	expectedGuard := st.armedAt.Add(500 * time.Millisecond)
	if st.guardUntil != expectedGuard {
		t.Errorf("guardUntil should be armedAt + 500ms")
	}
}

func TestResetVADState(t *testing.T) {
	s := NewServer()
	st := &sessionState{
		speaking:     true,
		consecSpeech: 5,
		nonSpeech:    3,
	}

	s.resetVADState(st)

	if st.speaking {
		t.Error("speaking should be false after reset")
	}
	if st.consecSpeech != 0 {
		t.Error("consecSpeech should be 0 after reset")
	}
	if st.nonSpeech != 0 {
		t.Error("nonSpeech should be 0 after reset")
	}
}
