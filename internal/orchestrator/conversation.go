package orchestrator

import (
	"context"
	"log"
	"os"
	"time"

	llmpb "yuzu/agent/internal/llm/pb"
	gw "yuzu/agent/internal/orchestrator/pb"
)

// handleTTSEvent processes TTS lifecycle events from the gateway.
func (s *Server) handleTTSEvent(st *sessionState, ttsType string, firstAudioMs uint32) {
	switch ttsType {
	case "started":
		// Re-arm barge-in guard window
		s.armBargeIn(st, 500, uint32(st.minRMS))
		s.resetVADState(st)
		s.setState(st, "SPEAKING")

	case "first_audio":
		if firstAudioMs > 0 {
			metricTTSFirstAudio.Observe(float64(firstAudioMs))
		}

	case "stopped":
		s.setState(st, "LISTENING")
	}
}

// handleTranscriptFinal processes final transcript and starts LLM.
func (s *Server) handleTranscriptFinal(ctx context.Context, st *sessionState, sid string, text string, send func(*gw.OrchestratorCommand)) {
	s.setState(st, "PROCESSING")
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

	msgs := []*llmpb.ChatMessage{}
	if sys != "" {
		msgs = append(msgs, &llmpb.ChatMessage{Role: "system", Content: sys})
	}
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
		log.Printf("[orch] llm session: %v", err)
		cancel()
		return
	}

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
