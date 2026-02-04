package orchestrator

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"

	llmpb "yuzu/agent/internal/llm/pb"
	gw "yuzu/agent/internal/orchestrator/pb"
)

// sessionState holds per-session state.
type sessionState struct {
	id    string
	state string // IDLE, LISTENING, PROCESSING, SPEAKING

	// VAD state
	speaking     bool
	consecSpeech int
	nonSpeech    int
	minStart     int
	hangover     int
	minRMS       float64
	guardUntil   time.Time
	armedAt      time.Time

	// Agreement tracking
	lastFeatureStart time.Time
	lastGatewayStart time.Time

	// LLM streaming state
    llmCancel context.CancelFunc
    llmActive bool

    // LLM latency tracking
    lastTranscriptFinal time.Time
    llmFirstSentence    bool
}

// Server implements the GatewayControl gRPC service.
type Server struct {
	gw.UnimplementedGatewayControlServer
	mu        sync.Mutex
	sess      map[string]*sessionState
	vadSource string // "feature" | "gateway"

	// Persistent LLM client
	llmMu     sync.RWMutex
	llmConn   *grpc.ClientConn
	llmClient llmpb.LLMClient
}

// NewServer creates a new orchestrator server.
func NewServer() *Server {
	src := os.Getenv("ORCH_VAD_SOURCE")
	if src == "" {
		src = "feature"
	}
	return &Server{
		sess:      make(map[string]*sessionState),
		vadSource: src,
	}
}

// Session handles the bidirectional gRPC stream with the gateway.
func (s *Server) Session(stream gw.GatewayControl_SessionServer) error {
	ctx := stream.Context()
	send := func(cmd *gw.OrchestratorCommand) { _ = stream.Send(cmd) }

	for {
		ev, err := stream.Recv()
		if err != nil {
			log.Printf("[orch] session stream.Recv error: %v", err)
			return err
		}

		sid := ev.GetSessionId()
		if sid == "" {
			sid = "unknown"
		}

		st := s.getOrCreateSession(sid)

		switch x := ev.Evt.(type) {
		case *gw.GatewayEvent_SessionOpen:
			s.handleSessionOpen(st, sid, x.SessionOpen.GetRoomUrl(), stream)

		case *gw.GatewayEvent_Feature:
			rms := float64(x.Feature.GetRms())
			s.processFeature(st, rms, time.Now(), sid, stream)

		case *gw.GatewayEvent_VadStart:
			s.processGatewayVAD(st, time.Now(), sid, stream)

		case *gw.GatewayEvent_VadEnd:
			// No-op for now

		case *gw.GatewayEvent_Tts:
			s.handleTTSEvent(st, x.Tts.GetType(), x.Tts.GetFirstAudioMs())

		case *gw.GatewayEvent_TranscriptInterim:
			// Could log or update UI

		case *gw.GatewayEvent_TranscriptFinal:
			log.Printf("[orch] Received TranscriptFinal event sid=%s text=%q", sid, x.TranscriptFinal.GetText())
			s.handleTranscriptFinal(ctx, st, sid, x.TranscriptFinal.GetText(), send)

		case *gw.GatewayEvent_Error:
			log.Printf("[orch] gateway error sid=%s code=%s msg=%s",
				sid, x.Error.GetCode(), x.Error.GetMessage())

		default:
			// Ignore unknown events for forward compatibility
		}
	}
}

// handleSessionOpen initializes a new session.
func (s *Server) handleSessionOpen(st *sessionState, sid string, roomURL string, stream gw.GatewayControl_SessionServer) {
	log.Printf("[orch] session_open id=%s room=%s", sid, roomURL)

	if st.state == "" {
		s.setState(st, "IDLE")
	}

	// Configure barge-in thresholds but don't arm yet - wait for TTS first_audio
	guardMs := uint32(envInt("LOCAL_STOP_GUARD_MS", 1000))
	minRms := uint32(envInt("LOCAL_STOP_MIN_RMS", 1200))
	// Store minRMS in session state so it's available when first_audio arms barge-in
	st.minRMS = float64(minRms)
	// Set guard to distant future - will be properly armed on first_audio
	st.guardUntil = time.Now().Add(24 * time.Hour)
	log.Printf("[orch] session_open configured minRMS=%.0f, barge-in will arm on first_audio", st.minRMS)

	// Notify gateway of barge-in config
	s.sendCmd(stream, &gw.OrchestratorCommand{
		SessionId: sid,
		Cmd: &gw.OrchestratorCommand_ArmBargeIn{
			ArmBargeIn: &gw.ArmBargeIn{GuardMs: guardMs, MinRms: minRms},
		},
	})

	// Enable mic to STT
	s.sendCmd(stream, &gw.OrchestratorCommand{
		SessionId: sid,
		Cmd:       &gw.OrchestratorCommand_StartMicToStt{StartMicToStt: &gw.StartMicToSTT{}},
	})
}

// getOrCreateSession returns existing session or creates a new one.
func (s *Server) getOrCreateSession(sid string) *sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.sess[sid]
	if st == nil {
		st = &sessionState{
			id:       sid,
			minStart: 2,
			hangover: 20,
			minRMS:   1200.0,
		}
		s.sess[sid] = st
	}
	return st
}

// setState transitions session state and records metric.
func (s *Server) setState(st *sessionState, to string) {
	from := st.state
	if from == to {
		return
	}
	metricStateTransitions.WithLabelValues(from, to).Inc()
	st.state = to
}

// sendCmd sends a command to the gateway, logging on failure.
func (s *Server) sendCmd(stream gw.GatewayControl_SessionServer, cmd *gw.OrchestratorCommand) bool {
	if err := stream.Send(cmd); err != nil {
		log.Printf("[orch] send failed sid=%s cmd=%T: %v", cmd.GetSessionId(), cmd.Cmd, err)
		return false
	}
	return true
}

// envInt reads an environment variable as int, returning def if not set or invalid.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
