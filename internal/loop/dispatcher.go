package loop

import (
    "context"
    "sync"
    "time"

    "github.com/google/uuid"
    "yuzu/agent/internal/floor"
    "yuzu/agent/internal/store"
    "yuzu/agent/internal/workerws"
)

type Dispatcher struct {
    reg   *workerws.Registry
    store *store.Store

    ttsTimeoutSec int

    mu       sync.Mutex
    sessions map[string]*sessState
}

type sessState struct {
    fsm           *floor.Manager
    lastVADTsMs   int64
    lastVADRecvMs int64
    stopping      bool
    pendingCmdID  string
    ttsStartRecv  time.Time
    bargeInArmed  bool
}

func New(reg *workerws.Registry, st *store.Store, ttsTimeoutSec int) *Dispatcher {
    return &Dispatcher{reg: reg, store: st, ttsTimeoutSec: ttsTimeoutSec, sessions: make(map[string]*sessState)}
}

func (d *Dispatcher) state(sessionID string) *sessState {
    d.mu.Lock()
    defer d.mu.Unlock()
    s := d.sessions[sessionID]
    if s == nil {
        s = &sessState{fsm: floor.New()}
        d.sessions[sessionID] = s
    }
    return s
}

// OnMessage processes a worker message and may send commands to the worker.
func (d *Dispatcher) OnMessage(sessionID string, msg workerws.Message) {
    s := d.state(sessionID)
    nowRecvMs := time.Now().UnixMilli()

    switch msg.Type {
    case "tts_started":
        s.fsm.OnTTSStarted(msg.UtteranceID, msg.TsMs)
        s.ttsStartRecv = time.Now()
        s.bargeInArmed = false
        d.store.AppendEvent(sessionID, "tts_started_backend_recv", map[string]any{"recv_ms": nowRecvMs})
    case "tts_first_audio":
        // Arm barge-in only after first audio is emitted, to avoid prebuffer cut-offs
        s.bargeInArmed = true
        d.store.AppendEvent(sessionID, "tts_first_audio_backend_recv", map[string]any{"recv_ms": nowRecvMs})
    case "tts_stopped":
        reason := ""
        if msg.Payload != nil {
            if v, ok := msg.Payload["reason"].(string); ok { reason = v }
        }
        s.fsm.OnTTSStopped(msg.UtteranceID, msg.TsMs, reason)
        s.bargeInArmed = false
        // If interrupted, compute latency
        if reason == "interrupted" && s.lastVADTsMs > 0 {
            workerMs := msg.TsMs - s.lastVADTsMs
            backendMs := nowRecvMs - s.lastVADRecvMs
            d.store.AppendEvent(sessionID, "barge_in_latency", map[string]any{
                "worker_ms": workerMs, "backend_ms": backendMs,
                "utterance_id": msg.UtteranceID, "vad_ts_ms": s.lastVADTsMs, "tts_stop_ts_ms": msg.TsMs,
                "recv_vad_ms": s.lastVADRecvMs, "recv_tts_stop_ms": nowRecvMs,
            })
        }
        s.stopping = false
        s.pendingCmdID = ""
    case "vad_start":
        s.lastVADTsMs = msg.TsMs
        s.lastVADRecvMs = nowRecvMs
        // Only treat candidate_audio (or debug) as barge-in sources
        source := ""
        if msg.Payload != nil {
            if v, ok := msg.Payload["source"].(string); ok { source = v }
        }
        dec := s.fsm.OnVADStart(msg.TsMs)
        if s.bargeInArmed && (source == "candidate_audio" || source == "debug") && dec.ShouldStop && !s.stopping {
            s.stopping = true
            cmdID := uuid.New().String()
            s.pendingCmdID = cmdID
            // Send stop_tts to worker
            out := workerws.Message{
                Type:        "stop_tts",
                TsMs:        time.Now().UnixMilli(),
                SessionID:   sessionID,
                Seq:         0,
                CommandID:   cmdID,
                UtteranceID: dec.StopUtteranceID,
                Payload:     map[string]any{"mode": "current"},
            }
            // Best-effort send; append event regardless
            ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
            _ = d.reg.SendJSON(ctx, sessionID, out)
            cancel()
            d.store.AppendEvent(sessionID, "stop_tts_sent", map[string]any{"command_id": cmdID, "utterance_id": dec.StopUtteranceID})
        }
    case "vad_end":
        s.fsm.OnVADEnd(msg.TsMs)
    case "cmd_ack":
        if msg.CommandID != "" && msg.CommandID == s.pendingCmdID {
            d.store.AppendEvent(sessionID, "cmd_ack", map[string]any{"command_id": msg.CommandID})
        } else {
            d.store.AppendEvent(sessionID, "cmd_ack", map[string]any{"command_id": msg.CommandID, "note": "unexpected"})
        }
    case "worker_hello":
        // Reset speaking unless worker immediately restates playback
        s.fsm = floor.New()
        s.stopping = false
        s.pendingCmdID = ""
    }

    // Safety timeout check
    if !s.ttsStartRecv.IsZero() && time.Since(s.ttsStartRecv) > time.Duration(d.ttsTimeoutSec)*time.Second {
        // Reset
        s.fsm = floor.New()
        s.stopping = false
        s.pendingCmdID = ""
        s.ttsStartRecv = time.Time{}
        d.store.AppendEvent(sessionID, "tts_timeout_reset", nil)
    }
}
