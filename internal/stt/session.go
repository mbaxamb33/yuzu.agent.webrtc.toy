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
    lastFinalText string
    lastSpeechStarted time.Time
    lastUtteranceEndAt time.Time
    lastInterimAt time.Time
    inUtterance bool
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
            now := time.Now()
            // Guardrail: if finalEmitted is stuck true and we've been seeing interims for > X ms, force reset
            // This handles cases where UtteranceEnd was missed/dropped
            if s.finalEmitted && !s.lastInterimAt.IsZero() {
                stuckMs := 1200
                if v := os.Getenv("STT_STUCK_FINAL_RESET_MS"); v != "" { fmt.Sscanf(v, "%d", &stuckMs) }
                if now.Sub(s.lastInterimAt) < time.Duration(stuckMs)*time.Millisecond {
                    // We've been getting interims continuously - check how long since final was emitted
                    // Use startedAt as a proxy for when the final was emitted
                    if now.Sub(s.startedAt) >= time.Duration(stuckMs)*time.Millisecond {
                        log.Printf("[stt] GUARDRAIL: forcing reset of stuck finalEmitted after %dms of interims session=%s", stuckMs, s.id)
                        s.finalEmitted = false
                        s.lastFinalText = ""
                        s.inUtterance = false
                        metricUtteranceEvents.WithLabelValues("guardrail_reset").Inc()
                    }
                }
            }
            // If idle (no active utterance), consider committing a new utterance based on silence and interim length
            if !s.inUtterance {
                minSil := 700
                if v := os.Getenv("MIN_SILENCE_FOR_NEW_UTTER_MS"); v != "" { fmt.Sscanf(v, "%d", &minSil) }
                minChars := 4
                if v := os.Getenv("MIN_INTERIM_CHARS_FOR_NEW_UTTER"); v != "" { fmt.Sscanf(v, "%d", &minChars) }
                prevInterimAt := s.lastInterimAt
                silenceOK := prevInterimAt.IsZero() || now.Sub(prevInterimAt) >= time.Duration(minSil)*time.Millisecond || (!s.lastUtteranceEndAt.IsZero() && now.Sub(s.lastUtteranceEndAt) >= 0)
                if len(strings.TrimSpace(e.Text)) >= minChars && silenceOK {
                    newID := fmt.Sprintf("utt-%d", now.UnixMilli())
                    log.Printf("[stt] committing new utterance on interim id=%s session=%s", newID, s.id)
                    s.StartUtterance(newID)
                    s.inUtterance = true
                }
            }
            log.Printf("[stt] interim transcript session=%s text=%q", s.id, e.Text)
            s.lastInterim = e.Text
            s.lastInterimAt = time.Now()
            if !s.seenFirstInterim && !s.startedAt.IsZero() {
                s.seenFirstInterim = true
                ms := time.Since(s.startedAt).Milliseconds()
                if ms > 0 { metricTTFTMS.Observe(float64(ms)) }
            }
            s.events <- &pb.ServerMessage{Msg: &pb.ServerMessage_Interim{Interim: &pb.TranscriptInterim{SessionId: s.id, UtteranceId: s.utterID, Text: e.Text}}}
        case "final":
            now := time.Now()
            log.Printf("[stt] final transcript received session=%s text=%q finalEmitted=%v", s.id, e.Text, s.finalEmitted)
            // Skip empty finals
            if strings.TrimSpace(e.Text) == "" {
                log.Printf("[stt] skipping empty final session=%s", s.id)
                continue
            }
            // If we already emitted a final for the current utterance, decide if this is a new utterance.
            if s.finalEmitted {
                // If exact duplicate of last final, drop as duplicate.
                if s.lastFinalText == e.Text {
                    log.Printf("[stt] skipping duplicate final session=%s (same text)", s.id)
                    continue
                }
                // Narrow rollover: require recent boundary or silence gap before creating a new utterance
                minSil := 700
                if v := os.Getenv("MIN_SILENCE_FOR_NEW_UTTER_MS"); v != "" { fmt.Sscanf(v, "%d", &minSil) }
                boundaryOK := !s.lastUtteranceEndAt.IsZero() && now.Sub(s.lastUtteranceEndAt) <= 3*time.Second
                silenceOK := s.lastInterimAt.IsZero() || now.Sub(s.lastInterimAt) >= time.Duration(minSil)*time.Millisecond
                if boundaryOK || silenceOK {
                    newID := fmt.Sprintf("utt-%d", now.UnixMilli())
                    log.Printf("[stt] rolling to new utterance for subsequent final; new id=%s session=%s", newID, s.id)
                    s.StartUtterance(newID)
                } else {
                    log.Printf("[stt] skipping subsequent final (no boundary/silence) session=%s", s.id)
                    continue
                }
            }
            if !s.drainAt.IsZero() {
                ms := time.Since(s.drainAt).Milliseconds()
                if ms > 0 { metricFinalLatencyMS.Observe(float64(ms)) }
            }
            log.Printf("[stt] FORWARDING final to gateway session=%s text=%q utterance=%s", s.id, e.Text, s.utterID)
            s.events <- &pb.ServerMessage{Msg: &pb.ServerMessage_Final{Final: &pb.TranscriptFinal{SessionId: s.id, UtteranceId: s.utterID, Text: e.Text}}}
            s.finalEmitted = true
            s.lastFinalText = e.Text
        case "error":
            log.Printf("[stt] error session=%s msg=%s", s.id, e.Text)
            s.events <- &pb.ServerMessage{Msg: &pb.ServerMessage_Error{Error: &pb.Error{SessionId: s.id, EnumCode: pb.ErrorCode_PROVIDER_ERROR, Message: e.Text}}}
        case "reconnected":
            // Defensive reset on provider reconnect
            log.Printf("[stt] provider reconnected; resetting session state session=%s", s.id)
            s.finalEmitted = false
            s.lastFinalText = ""
            s.lastInterim = ""
            s.seenFirstInterim = false
            s.startedAt = time.Now()
            s.inUtterance = false
            s.lastUtteranceEndAt = time.Now()
            metricUtteranceEvents.WithLabelValues("guardrail_reset").Inc()
        case "utterance_end":
            // Reset gating so subsequent utterances can be transcribed
            log.Printf("[stt] utterance_end received, resetting gating session=%s (finalEmitted was %v)", s.id, s.finalEmitted)
            s.finalEmitted = false
            s.lastInterim = ""
            s.seenFirstInterim = false
            s.startedAt = time.Now()
            s.lastFinalText = ""
            s.inUtterance = false
            s.lastUtteranceEndAt = time.Now()
            metricUtteranceEvents.WithLabelValues("utterance_end").Inc()
        case "speech_started":
            // Treat SpeechStarted as a hint only; log/metric, do not segment on it
            now := time.Now()
            if !s.lastSpeechStarted.IsZero() && now.Sub(s.lastSpeechStarted) < 250*time.Millisecond {
                log.Printf("[stt] speech_started ignored (debounced) session=%s", s.id)
                break
            }
            s.lastSpeechStarted = now
            log.Printf("[stt] speech_started hint session=%s", s.id)
            metricUtteranceEvents.WithLabelValues("speech_started").Inc()
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
    s.inUtterance = true
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
