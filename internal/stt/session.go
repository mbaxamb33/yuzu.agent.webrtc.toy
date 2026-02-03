package stt

import (
    "context"
    "fmt"
    "log"
    "math"
    "os"
    "strings"
    "sync"
    "time"

    pb "yuzu/agent/internal/stt/pb"
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
            log.Printf("[stt] interim transcript session=%s text=%q", s.id, e.Text)
            s.lastInterim = e.Text
            if !s.seenFirstInterim && !s.startedAt.IsZero() {
                s.seenFirstInterim = true
                ms := time.Since(s.startedAt).Milliseconds()
                if ms > 0 { metricTTFTMS.Observe(float64(ms)) }
            }
            s.events <- &pb.ServerMessage{Msg: &pb.ServerMessage_Interim{Interim: &pb.TranscriptInterim{SessionId: s.id, UtteranceId: s.utterID, Text: e.Text}}}
        case "final":
            log.Printf("[stt] final transcript received session=%s text=%q finalEmitted=%v", s.id, e.Text, s.finalEmitted)
            // Skip empty finals and already-emitted finals
            if e.Text == "" {
                log.Printf("[stt] skipping empty final session=%s", s.id)
                continue
            }
            if s.finalEmitted {
                log.Printf("[stt] skipping duplicate final session=%s (already emitted)", s.id)
                continue
            }
            if !s.drainAt.IsZero() {
                ms := time.Since(s.drainAt).Milliseconds()
                if ms > 0 { metricFinalLatencyMS.Observe(float64(ms)) }
            }
            log.Printf("[stt] FORWARDING final to gateway session=%s text=%q utterance=%s", s.id, e.Text, s.utterID)
            s.events <- &pb.ServerMessage{Msg: &pb.ServerMessage_Final{Final: &pb.TranscriptFinal{SessionId: s.id, UtteranceId: s.utterID, Text: e.Text}}}
            s.finalEmitted = true
        case "error":
            log.Printf("[stt] error session=%s msg=%s", s.id, e.Text)
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
    // Calculate RMS for audio level diagnostics
    rms := calcRMS(b)
    if s.framesIn == 1 || s.framesIn%50 == 0 {
        log.Printf("[stt] audio session=%s frame=%d bytes=%d rms=%.0f queueLen=%d", s.id, s.framesIn, len(b), rms, s.dg.QueueLen())
    }
    // Save first high-RMS audio sample for format verification
    if s.framesIn <= 500 && rms > 500 {
        filename := fmt.Sprintf("/tmp/stt_audio_sample_%s_frame%d_rms%.0f.raw", s.id[:8], s.framesIn, rms)
        _ = os.WriteFile(filename, b, 0644)
        log.Printf("[stt] saved audio sample: %s", filename)
    }
    // drop-latest policy if DG queue is congested
    ok := s.dg.Send(b)
    if !ok {
        metricDrops.Inc()
        log.Printf("[stt] DROPPED frame=%d rms=%.0f queueLen=%d", s.framesIn, rms, s.dg.QueueLen())
    }
    metricAudioBytes.Add(float64(len(b)))
    metricFrames.Inc()
    gaugeQueueDepth.Set(float64(s.dg.QueueLen()))
}

// calcRMS computes RMS of PCM16 audio
func calcRMS(b []byte) float64 {
    if len(b) < 2 {
        return 0
    }
    var sum float64
    n := len(b) / 2
    for i := 0; i < n; i++ {
        // Little-endian int16
        sample := int16(uint16(b[i*2]) | uint16(b[i*2+1])<<8)
        sum += float64(sample) * float64(sample)
    }
    return math.Sqrt(sum / float64(n))
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
