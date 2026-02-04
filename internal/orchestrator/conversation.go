package orchestrator

import (
    "context"
    "log"
    "os"
    "time"

    llmpb "yuzu/agent/internal/llm/pb"
    gw "yuzu/agent/internal/orchestrator/pb"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
)

// handleTTSEvent processes TTS lifecycle events from the gateway.
func (s *Server) handleTTSEvent(st *sessionState, ttsType string, firstAudioMs uint32) {
	log.Printf("[orch] TTS event received type=%s sid=%s", ttsType, st.id)
	switch ttsType {
	case "started":
		// Just reset VAD state and mark speaking - don't arm barge-in yet
		// Barge-in will be armed on first_audio when audio actually plays
		s.resetVADState(st)
		s.setState(st, "SPEAKING")
		log.Printf("[orch] TTS started, waiting for first_audio to arm barge-in sid=%s", st.id)

	case "first_audio":
		// NOW arm barge-in - audio is actually playing
		guardMs := uint32(envInt("LOCAL_STOP_GUARD_MS", 1000))
		log.Printf("[orch] TTS first_audio, arming barge-in guard=%dms minRMS=%.0f sid=%s", guardMs, st.minRMS, st.id)
		s.armBargeIn(st, guardMs, uint32(st.minRMS))
		if firstAudioMs > 0 {
			metricTTSFirstAudio.Observe(float64(firstAudioMs))
		}

	case "stopped":
		s.setState(st, "LISTENING")
	}
}

// handleTranscriptFinal processes final transcript and starts LLM.
func (s *Server) handleTranscriptFinal(ctx context.Context, st *sessionState, sid string, text string, send func(*gw.OrchestratorCommand)) {
	log.Printf("[orch] TRANSCRIPT_FINAL received sid=%s text_len=%d text=%q state=%s", sid, len(text), text, st.state)
	s.setState(st, "PROCESSING")
	// Mark transcript final time for LLMSentence latency
	st.lastTranscriptFinal = time.Now()
	st.llmFirstSentence = false
	log.Printf("[orch] Starting LLM for sid=%s", sid)
	go s.startLLM(ctx, sid, text, send)
}

// startLLM starts an LLM streaming request and forwards sentences to Gateway as StartTTS.
func (s *Server) startLLM(parent context.Context, sessionID string, userText string, send func(*gw.OrchestratorCommand)) {
    // Resolve deployment and API version with Azure fallbacks
    deployment := os.Getenv("LLM_DEPLOYMENT")
    if deployment == "" {
        deployment = os.Getenv("AZURE_OPENAI_DEPLOYMENT")
    }
    apiVersion := os.Getenv("LLM_API_VERSION")
    if apiVersion == "" {
        apiVersion = os.Getenv("AZURE_OPENAI_API_VERSION")
    }
    if apiVersion == "" {
        apiVersion = "2024-02-15-preview"
    }
	sys := os.Getenv("LLM_SYSTEM_PROMPT")
	if sys == "" {
		// Default TTS-friendly prompt: concise, conversational, no formatting
		sys = "You are a friendly voice assistant. Respond in 1-2 short sentences. " +
			"Be conversational and natural. Never use bullet points, lists, markdown, " +
			"or special formatting. Your responses will be spoken aloud via text-to-speech."
	}

	msgs := []*llmpb.ChatMessage{}
	msgs = append(msgs, &llmpb.ChatMessage{Role: "system", Content: sys})
	msgs = append(msgs, &llmpb.ChatMessage{Role: "user", Content: userText})

	ctx, cancel := context.WithCancel(parent)
	client, err := s.getLLMClient(ctx)
	if err != nil {
		log.Printf("[orch] llm dial: %v", err)
		cancel()
		return
	}

    stream, err := client.Session(ctx)
    if err != nil {
        // Reconnect only on connection-level failures
        st, _ := status.FromError(err)
        if st != nil && (st.Code() == codes.Unavailable || st.Code() == codes.ResourceExhausted) {
            if rerr := s.reconnectLLM(ctx, 1); rerr == nil {
                if client, r2 := s.getLLMClient(ctx); r2 == nil {
                    if stream, err = client.Session(ctx); err == nil {
                        goto STREAM
                    }
                }
            }
        }
        log.Printf("[orch] llm session: %v", err)
        cancel()
        return
    }
STREAM:

	s.attachLLM(sessionID, cancel)

	// Send start request
	err = stream.Send(&llmpb.ClientMessage{
		Msg: &llmpb.ClientMessage_Start{
			Start: &llmpb.StartRequest{
				SessionId:  sessionID,
				RequestId:  time.Now().Format("20060102150405.000"),
				Deployment: deployment,
				ApiVersion: apiVersion,
				Messages:   msgs,
				Stream:     true,
			},
		},
	})
	if err != nil {
		log.Printf("[orch] llm send start: %v", err)
		cancel()
		s.detachLLM(sessionID)
		return
	}

	// Read responses in background
    go s.streamLLMResponses(stream, sessionID, send, cancel)
}

// streamLLMResponses reads LLM stream and forwards sentences to TTS.
func (s *Server) streamLLMResponses(stream llmpb.LLM_SessionClient, sessionID string, send func(*gw.OrchestratorCommand), cancel context.CancelFunc) {
	defer func() {
		cancel()
		s.detachLLM(sessionID)
	}()

	for {
		resp, err := stream.Recv()
        if err != nil {
            // Stream closed (normal or cancelled)
            return
        }

		switch m := resp.Msg.(type) {
        case *llmpb.ServerMessage_Sentence:
            text := m.Sentence.GetText()
            if text != "" {
                log.Printf("[orch] LLM sentence received sid=%s text_len=%d text=%q", sessionID, len(text), text)
                // Observe LLMSentence latency on first sentence since final
                s.mu.Lock()
                if st, ok := s.sess[sessionID]; ok && !st.llmFirstSentence && !st.lastTranscriptFinal.IsZero() {
                    d := time.Since(st.lastTranscriptFinal)
                    if d > 0 { metricLLMSentenceLatency.Observe(float64(d.Milliseconds())) }
                    st.llmFirstSentence = true
                }
                s.mu.Unlock()
                log.Printf("[orch] Sending StartTTS command to gateway sid=%s text_len=%d", sessionID, len(text))
                send(&gw.OrchestratorCommand{
                    SessionId: sessionID,
                    Cmd:       &gw.OrchestratorCommand_StartTts{StartTts: &gw.StartTTS{Text: text}},
                })
            }

		case *llmpb.ServerMessage_Error:
			log.Printf("[orch] llm error: %s", m.Error.GetMessage())

		case *llmpb.ServerMessage_Usage:
			// Could emit metrics here
		}
	}
}
