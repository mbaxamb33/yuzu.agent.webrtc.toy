package floor

// Decision represents the action the floor manager wants to take.
type Decision struct {
    ShouldStop      bool
    StopUtteranceID string
    Reason          string // e.g., "barge_in"
}

type Manager struct {
    speaking           bool
    activeUtteranceID  string
    lastVADStartTsMs   int64
    lastTTSStartedTsMs int64
}

func New() *Manager { return &Manager{} }

func (m *Manager) OnTTSStarted(utteranceID string, tsMs int64) Decision {
    m.speaking = true
    m.activeUtteranceID = utteranceID
    m.lastTTSStartedTsMs = tsMs
    return Decision{}
}

func (m *Manager) OnTTSStopped(utteranceID string, tsMs int64, reason string) Decision {
    // Regardless of ID match, stopping clears speaking.
    m.speaking = false
    m.activeUtteranceID = ""
    return Decision{}
}

func (m *Manager) OnVADStart(tsMs int64) Decision {
    m.lastVADStartTsMs = tsMs
    if m.speaking {
        // barge-in: stop immediately
        return Decision{ShouldStop: true, StopUtteranceID: m.activeUtteranceID, Reason: "barge_in"}
    }
    return Decision{}
}

func (m *Manager) OnVADEnd(tsMs int64) Decision {
    return Decision{}
}

