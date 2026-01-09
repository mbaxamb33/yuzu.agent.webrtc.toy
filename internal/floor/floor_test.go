package floor

import "testing"

func TestBargeInTriggersStop(t *testing.T) {
    f := New()
    f.OnTTSStarted("u1", 1000)
    d := f.OnVADStart(1500)
    if !d.ShouldStop || d.Reason != "barge_in" || d.StopUtteranceID != "u1" {
        t.Fatalf("expected stop on barge-in, got %+v", d)
    }
}

func TestVADIdleDoesNothing(t *testing.T) {
    f := New()
    d := f.OnVADStart(1000)
    if d.ShouldStop {
        t.Fatalf("should not stop when idle")
    }
}

func TestTTSStoppedClearsSpeaking(t *testing.T) {
    f := New()
    f.OnTTSStarted("u1", 1000)
    f.OnTTSStopped("u1", 2000, "completed")
    d := f.OnVADStart(2500)
    if d.ShouldStop {
        t.Fatalf("should not request stop after tts stopped")
    }
}

