package stt

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "net/url"
    "os"
    "strings"
    "time"

    "nhooyr.io/websocket"
)

// DeepgramConn maintains a single live websocket connection to Deepgram
// for a session, sending PCM16@16k audio and receiving transcript events.
type DeepgramConn struct {
    ctx    context.Context
    cancel context.CancelFunc

    apiKey string
    url    string

    ws *websocket.Conn

    // Outbound audio queue; caller should drop-latest upstream on pressure
    sendQ chan []byte
    // Events channel emits interim/final transcripts
    Events chan DGEvent

    // Backoff/circuit
    fails    []time.Time
    circuit  time.Time
    maxAge   time.Duration

    // Track last interim/final text for UtteranceEnd fallback
    lastText      string
    lastFinalText string
}

type DGEvent struct {
    Type        string // "interim" | "final" | "error"
    UtteranceID string
    Text        string
    Raw         map[string]any
}

type DGConfig struct {
    Model          string
    Language       string
    EndpointingMs  int
    Interim        bool
    UtterEndMs     int
    VADEvents      bool
    BaseURL        string
    SocketMaxAgeS  int
}

func NewDeepgramConn(parent context.Context, cfg DGConfig, apiKey string) *DeepgramConn {
    ctx, cancel := context.WithCancel(parent)
    q := url.Values{}
    q.Set("model", orDefault(cfg.Model, "nova-2"))
    q.Set("language", orDefault(cfg.Language, "en-US"))
    q.Set("smart_format", "true")
    q.Set("endpointing", fmt.Sprintf("%d", nzd(cfg.EndpointingMs, 1000)))
    q.Set("interim_results", fmt.Sprintf("%t", cfg.Interim))
    q.Set("utterance_end_ms", fmt.Sprintf("%d", nzd(cfg.UtterEndMs, 1500)))
    q.Set("vad_events", fmt.Sprintf("%t", cfg.VADEvents))
    q.Set("encoding", "linear16")
    q.Set("sample_rate", "16000")
    q.Set("channels", "1")
    base := cfg.BaseURL
    if base == "" {
        base = "wss://api.deepgram.com/v1/listen"
    }
    return &DeepgramConn{
        ctx:    ctx,
        cancel: cancel,
        apiKey: apiKey,
        url:    base + "?" + q.Encode(),
        sendQ:  make(chan []byte, 8),
        Events: make(chan DGEvent, 32),
        maxAge: time.Duration(nzd(cfg.SocketMaxAgeS, 900)) * time.Second,
    }
}

func (d *DeepgramConn) Start() {
    go d.run()
}

func (d *DeepgramConn) Close() { d.cancel() }

func (d *DeepgramConn) Send(pcm16k []byte) bool {
    select {
    case d.sendQ <- pcm16k:
        return true
    default:
        return false
    }
}

func (d *DeepgramConn) QueueLen() int { return len(d.sendQ) }

func (d *DeepgramConn) run() {
    defer close(d.Events)
    for {
        if err := d.connectAndPump(); err != nil {
            d.addFailure()
            // emit error event so caller may choose to degrade
            d.emit(DGEvent{Type: "error", Text: err.Error()})
        } else {
            d.resetFailures()
        }
        if d.ctx.Err() != nil {
            return
        }
        // backoff
        time.Sleep(d.nextBackoff())
    }
}

func (d *DeepgramConn) connectAndPump() error {
    // circuit breaker
    if time.Now().Before(d.circuit) {
        time.Sleep(500 * time.Millisecond)
        return fmt.Errorf("circuit open")
    }

    hdr := make(http.Header)
    if d.apiKey != "" {
        hdr.Set("Authorization", "Token "+d.apiKey)
    }
    ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
    defer cancel()
    start := time.Now()
    log.Printf("[deepgram] connecting to %s (apiKey len=%d)", d.url, len(d.apiKey))
    ws, _, err := websocket.Dial(ctx, d.url, &websocket.DialOptions{HTTPHeader: hdr})
    if err != nil {
        log.Printf("[deepgram] connect error: %v", err)
        return err
    }
    log.Printf("[deepgram] connected in %dms", time.Since(start).Milliseconds())
    metricConnectMS.Observe(float64(time.Since(start).Milliseconds()))
    metricReconnects.Inc()
    d.ws = ws
    defer func() {
        _ = d.ws.Close(websocket.StatusNormalClosure, "bye")
        d.ws = nil
    }()

    // Start send and recv loops
    sendDone := make(chan struct{})
    var bytesSent uint64
    var framesSent uint64
    go func() {
        defer close(sendDone)
        for {
            select {
            case <-d.ctx.Done():
                return
            case b := <-d.sendQ:
                if b == nil {
                    continue
                }
                wctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
                err := d.ws.Write(wctx, websocket.MessageBinary, b)
                cancel()
                if err != nil {
                    log.Printf("[deepgram] write error: %v", err)
                    return
                }
                bytesSent += uint64(len(b))
                framesSent++
                if framesSent == 1 || framesSent%100 == 0 {
                    // Log first 16 bytes hex for format verification
                    hexPrefix := ""
                    if len(b) >= 16 {
                        hexPrefix = fmt.Sprintf(" hex[0:16]=%x", b[:16])
                    }
                    log.Printf("[deepgram] sent frames=%d bytes=%d%s", framesSent, bytesSent, hexPrefix)
                }
            }
        }
    }()

    // schedule rotation if maxAge set
    var rotate <-chan time.Time
    if d.maxAge > 0 {
        t := time.NewTimer(d.maxAge)
        defer t.Stop()
        rotate = t.C
    }

    for {
        if d.ctx.Err() != nil {
            return nil
        }
        // non-blocking rotation check
        select {
        case <-rotate:
            return fmt.Errorf("rotate")
        default:
        }
        _, data, err := d.ws.Read(d.ctx)
        if err != nil {
            return err
        }
        // Expect JSON text frames
        if len(data) == 0 {
            continue
        }
        var m map[string]any
        if err := json.Unmarshal(data, &m); err != nil {
            log.Printf("[deepgram] JSON parse error: %v, data: %s", err, string(data[:min(200, len(data))]))
            continue
        }
        // Debug: log full raw response (truncated)
        rawStr := string(data)
        if len(rawStr) > 500 {
            rawStr = rawStr[:500] + "..."
        }
        log.Printf("[deepgram] recv raw: %s", rawStr)
        // Parse Deepgram results shape leniently
        // Look for results.alternatives[0].transcript and results.is_final
        typ := toString(m["type"]) // may be "Results", "UtteranceEnd", "Metadata", "Error"
        if strings.EqualFold(typ, "Error") || m["error"] != nil {
            // Provider error frame
            msg := toString(m["error"]) 
            if msg == "" { msg = toString(m["message"]) }
            if msg == "" { msg = "provider_error" }
            d.emit(DGEvent{Type: "error", Text: msg, Raw: m})
            continue
        }
        if strings.EqualFold(typ, "Metadata") {
            // Connection confirmation; optional (don't check m["metadata"] - all messages have it!)
            d.emit(DGEvent{Type: "meta", Raw: m})
            continue
        }
        if strings.EqualFold(typ, "SpeechStarted") {
            // Don't reset here - SpeechStarted can arrive immediately after a final
            // but before we've had a chance to forward it. Reset happens on UtteranceEnd instead.
            log.Printf("[deepgram] SpeechStarted detected (text tracking preserved)")
        } else if strings.EqualFold(typ, "Results") || m["channel"] != nil {
            // Deepgram puts alternatives under "channel", not "results"
            var channel map[string]any
            if v, ok := m["channel"].(map[string]any); ok {
                channel = v
            }
            var alts []any
            if channel != nil {
                if a, ok := channel["alternatives"].([]any); ok {
                    alts = a
                }
            }
            text := ""
            if len(alts) > 0 {
                if a0, ok := alts[0].(map[string]any); ok {
                    text = strings.TrimSpace(toString(a0["transcript"])) // Trim whitespace
                }
            }
            isFinal := toBool(m["is_final"]) || toBool(m["speech_final"])
            log.Printf("[deepgram] parsed: text=%q is_final=%v speech_final=%v type=%v alts_len=%d",
                text, toBool(m["is_final"]), toBool(m["speech_final"]), m["type"], len(alts))
            // Track text for UtteranceEnd fallback
            if text != "" {
                d.lastText = text
            }
            if isFinal {
                if text != "" {
                    d.lastFinalText = text
                    log.Printf("[deepgram] emitting FINAL source=provider text=%q", text)
                    d.emit(DGEvent{Type: "final", Text: text, Raw: m})
                    metricFinalEmitted.WithLabelValues("provider").Inc()
                } else {
                    log.Printf("[deepgram] skipping empty is_final result")
                    metricEmptyFinalSkipped.Inc()
                }
            } else {
                if text != "" {
                    d.emit(DGEvent{Type: "interim", Text: text, Raw: m})
                }
            }
        } else if strings.EqualFold(typ, "UtteranceEnd") {
            // UtteranceEnd signals end of speech - use last known text if we haven't emitted a final yet
            // This is a fallback in case is_final results were missed
            fallbackText := d.lastFinalText
            source := "provider_cached"
            if fallbackText == "" {
                fallbackText = d.lastText
                source = "interim_fallback"
            }
            log.Printf("[deepgram] UtteranceEnd received, fallback_text=%q source=%s lastFinal=%q lastText=%q",
                fallbackText, source, d.lastFinalText, d.lastText)
            // Emit UtteranceEnd as final - session.go will handle deduplication
            if fallbackText != "" {
                d.emit(DGEvent{Type: "final", Text: fallbackText, Raw: m})
                metricFinalEmitted.WithLabelValues(source).Inc()
            } else {
                log.Printf("[deepgram] UtteranceEnd with no text to emit")
                metricEmptyFinalSkipped.Inc()
            }
            // Reset tracking for next utterance
            d.lastText = ""
            d.lastFinalText = ""
        }
    }
}

func (d *DeepgramConn) emit(e DGEvent) {
    select {
    case d.Events <- e:
    default:
        // drop if slow consumer
    }
}

func (d *DeepgramConn) addFailure() {
    d.fails = append(d.fails, time.Now())
    // prune older than 60s
    cutoff := time.Now().Add(-60 * time.Second)
    j := 0
    for _, t := range d.fails {
        if t.After(cutoff) {
            d.fails[j] = t
            j++
        }
    }
    d.fails = d.fails[:j]
    if len(d.fails) >= 3 {
        d.circuit = time.Now().Add(30 * time.Second)
        metricCircuitOpens.Inc()
    }
}

func (d *DeepgramConn) resetFailures() { d.fails = nil }

func (d *DeepgramConn) nextBackoff() time.Duration {
    n := len(d.fails)
    if n <= 0 {
        return time.Second
    }
    if n > 5 {
        n = 5
    }
    base := time.Duration(1<<uint(n-1)) * time.Second
    if base > 30*time.Second {
        base = 30 * time.Second
    }
    return base
}

func orDefault(s, def string) string { if s == "" { return def }; return s }
func nzd(v, def int) int { if v == 0 { return def }; return v }
func toString(v any) string { if s, ok := v.(string); ok { return s }; return "" }
func toBool(v any) bool {
    switch t := v.(type) {
    case bool:
        return t
    case string:
        return strings.EqualFold(t, "true")
    default:
        return false
    }
}

func LoadDGConfigFromEnv() DGConfig {
    return DGConfig{
        Model:         os.Getenv("DEEPGRAM_MODEL"),
        Language:      os.Getenv("DEEPGRAM_LANGUAGE"),
        EndpointingMs: atoiEnv("DEEPGRAM_ENDPOINTING_MS", 1000),
        Interim:       true,
        UtterEndMs:    atoiEnv("DEEPGRAM_UTTERANCE_END_MS", 1500),
        VADEvents:     true,
        BaseURL:       os.Getenv("DEEPGRAM_WS_URL"),
    }
}

func atoiEnv(name string, def int) int {
    s := strings.TrimSpace(os.Getenv(name))
    if s == "" { return def }
    var x int
    _, err := fmt.Sscanf(s, "%d", &x)
    if err != nil { return def }
    return x
}
