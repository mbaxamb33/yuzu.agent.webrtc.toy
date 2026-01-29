package stt

import (
    "context"
    "os"
    "sync"
    "time"

    pb "yuzu/agent/internal/stt/pb"
    "strings"
)

// Session handles one client's stream, owning a DeepgramConn and queues.
type Session struct {
    mu     sync.Mutex
    ctx    context.Context
    cancel context.CancelFunc

    id        string
    utterID   string
    startedAt time.Time
    lastAct   time.Time

    dg     *DeepgramConn
    events chan *pb.ServerMessage

    bytesIn  uint64
    framesIn uint64
    lastMet  time.Time

    lastInterim string
    seenFirstInterim bool
    drainAt time.Time
    endpointPolicy string // "provider" | "earliest"
    finalEmitted bool
}

func NewSession(parent context.Context, sessionID string) *Session {
    ctx, cancel := context.WithCancel(parent)
    now := time.Now()
    s := &Session{ctx: ctx, cancel: cancel, id: sessionID, lastMet: now, lastAct: now}
    // Create Deepgram connection
    cfg := LoadDGConfigFromEnv()
    apiKey := os.Getenv("DEEPGRAM_API_KEY")
    s.dg = NewDeepgramConn(ctx, cfg, apiKey)
    pol := os.Getenv("STT_ENDPOINTING_POLICY")
    if pol == "" { pol = "provider" }
    s.endpointPolicy = pol
    s.events = make(chan *pb.ServerMessage, 64)
    go s.run()
    s.dg.Start()
    return s
}

func (s *Session) run() {
    // forward Deepgram events to gRPC layer
    for e := range s.dg.Events {
        switch e.Type {
        case "interim":
            s.lastInterim = e.Text
            if !s.seenFirstInterim && !s.startedAt.IsZero() {
                s.seenFirstInterim = true
                ms := time.Since(s.startedAt).Milliseconds()
                if ms > 0 { metricTTFTMS.Observe(float64(ms)) }
            }
            s.events <- &pb.ServerMessage{Msg: &pb.ServerMessage_Interim{Interim: &pb.TranscriptInterim{SessionId: s.id, UtteranceId: s.utterID, Text: e.Text}}}
        case "final":
            if s.finalEmitted { continue }
            if !s.drainAt.IsZero() {
                ms := time.Since(s.drainAt).Milliseconds()
                if ms > 0 { metricFinalLatencyMS.Observe(float64(ms)) }
            }
            s.events <- &pb.ServerMessage{Msg: &pb.ServerMessage_Final{Final: &pb.TranscriptFinal{SessionId: s.id, UtteranceId: s.utterID, Text: e.Text}}}
            s.finalEmitted = true
        case "error":
            s.events <- &pb.ServerMessage{Msg: &pb.ServerMessage_Error{Error: &pb.Error{SessionId: s.id, EnumCode: pb.ErrorCode_PROVIDER_ERROR, Message: e.Text}}}
        case "meta":
            // ignore or surface in future
        }
    }
    close(s.events)
}

func (s *Session) StartUtterance(utterID string) {
    s.mu.Lock()
    s.utterID = utterID
    s.startedAt = time.Now()
    s.lastAct = s.startedAt
    s.seenFirstInterim = false
    s.finalEmitted = false
    s.lastInterim = ""
    s.drainAt = time.Time{}
    s.mu.Unlock()
}

func (s *Session) SendAudio(b []byte) {
    s.bytesIn += uint64(len(b))
    s.framesIn++
    s.lastAct = time.Now()
    // drop-latest policy if DG queue is congested
    ok := s.dg.Send(b)
    if !ok {
        metricDrops.Inc()
    }
    metricAudioBytes.Add(float64(len(b)))
    metricFrames.Inc()
    gaugeQueueDepth.Set(float64(s.dg.QueueLen()))
}

func (s *Session) Drain() {
    // No explicit control for provider; rely on endpointing.
    s.lastAct = time.Now()
    s.drainAt = s.lastAct
    if strings.EqualFold(s.endpointPolicy, "earliest") && !s.finalEmitted {
        // Emit a synthesized final using last interim text
        s.events <- &pb.ServerMessage{Msg: &pb.ServerMessage_Final{Final: &pb.TranscriptFinal{SessionId: s.id, UtteranceId: s.utterID, Text: s.lastInterim}}}
        s.finalEmitted = true
        if !s.drainAt.IsZero() {
            ms := time.Since(s.drainAt).Milliseconds()
            if ms > 0 { metricFinalLatencyMS.Observe(float64(ms)) }
        }
    }
}

func (s *Session) Close() { s.cancel() }

// IdleFor returns true if the session has been idle for >= d.
func (s *Session) IdleFor(d time.Duration) bool {
    return time.Since(s.lastAct) >= d
}
