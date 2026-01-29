package stt

import (
    "context"
    "fmt"
    "os"
    "sync"
    "time"

    pb "yuzu/agent/internal/stt/pb"
)

// STTServer implements pb.STTServer.
type STTServer struct {
    pb.UnimplementedSTTServer
    ready bool
    mu    sync.Mutex
    sess  map[string]*Session
    idleTTL time.Duration
}

func NewSTTServer() *STTServer {
    s := &STTServer{ready: true, sess: make(map[string]*Session)}
    s.idleTTL = readIdleTTL()
    go s.reaper()
    return s
}
func (s *STTServer) Ready() bool { return s.ready }

// Session handles the gRPC bidi stream, routing to per-session state and provider.
func (s *STTServer) Session(stream pb.STT_SessionServer) error {
    ctx := stream.Context()
    var sess *Session
    var sessionID string
    // Metrics cadence
    var bytesIn, framesIn uint64
    lastMet := time.Now()

    // Non-blocking forwarder from provider → client
    var evCh <-chan *pb.ServerMessage
    send := func(msg *pb.ServerMessage) { _ = stream.Send(msg) }

    for {
        // forward any pending events
        if evCh != nil {
            select {
            case ev, ok := <-evCh:
                if ok {
                    send(ev)
                } else {
                    evCh = nil
                }
            default:
            }
        }

        msg, err := stream.Recv()
        if err != nil {
            if sess != nil { sess.Close() }
            return err
        }
        switch m := msg.Msg.(type) {
        case *pb.ClientMessage_Start:
            sessionID = m.Start.GetSessionId()
            utterID := m.Start.GetUtteranceId()
            s.mu.Lock()
            sess = s.sess[sessionID]
            if sess == nil {
                sess = NewSession(ctx, sessionID)
                s.sess[sessionID] = sess
                gaugeSessions.Inc()
            }
            s.mu.Unlock()
            sess.StartUtterance(utterID)
            send(&pb.ServerMessage{Msg: &pb.ServerMessage_Connected{Connected: &pb.Connected{SessionId: sessionID, Model: "nova-2"}}})
            if evCh == nil {
                evCh = sess.events
            }
        case *pb.ClientMessage_Audio:
            b := m.Audio.GetPcm16K()
            bytesIn += uint64(len(b))
            framesIn++
            if sess != nil && len(b) > 0 {
                sess.SendAudio(b)
            }
            if time.Since(lastMet) >= time.Second || framesIn%10 == 0 {
                send(&pb.ServerMessage{Msg: &pb.ServerMessage_Metrics{Metrics: &pb.Metrics{SessionId: sessionID, BytesSent: bytesIn, FramesSent: framesIn}}})
                lastMet = time.Now()
            }
        case *pb.ClientMessage_Drain:
            if sess != nil { sess.Drain() }
        case *pb.ClientMessage_Close:
            if sess != nil { sess.Close() }
            if sessionID != "" {
                s.mu.Lock()
                delete(s.sess, sessionID)
                s.mu.Unlock()
                gaugeSessions.Dec()
            }
            return nil
        case *pb.ClientMessage_Ping:
            send(&pb.ServerMessage{Msg: &pb.ServerMessage_Pong{Pong: &pb.Pong{Seq: m.Ping.Seq}}})
        default:
            // ignore
        }
    }
}

// GracefulShutdown flips readiness and can await draining work if needed.
func (s *STTServer) GracefulShutdown(ctx context.Context, timeout time.Duration) error {
    s.ready = false
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-time.After(timeout):
        return nil
    }
}

func (s *STTServer) reaper() {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()
    for range ticker.C {
        s.mu.Lock()
        ttl := s.idleTTL
        for id, sess := range s.sess {
            if sess.IdleFor(ttl) {
                sess.Close()
                delete(s.sess, id)
                gaugeSessions.Dec()
            }
        }
        s.mu.Unlock()
    }
}

func readIdleTTL() time.Duration {
    // Default 60s; read STT_SESSION_IDLE_TTL_S
    v := os.Getenv("STT_SESSION_IDLE_TTL_S")
    if v == "" { return 60 * time.Second }
    var n int
    if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 { return 60 * time.Second }
    return time.Duration(n) * time.Second
}
