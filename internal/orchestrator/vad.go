package orchestrator

import (
	"log"
	"time"

	gw "yuzu/agent/internal/orchestrator/pb"
)

// processFeature handles GatewayEvent_Feature based on vadSource config.
// Returns true if barge-in was triggered.
func (s *Server) processFeature(st *sessionState, rms float64, now time.Time, sid string, stream gw.GatewayControl_SessionServer) bool {
	metricVADFeatures.Inc()

	if s.vadSource != "feature" {
		// Secondary: record for agreement timing only
		s.recordFeatureAgreement(st, rms, now)
		return false
	}

	// Primary path: feature drives VAD
	return s.handleFeaturePrimary(st, rms, now, sid, stream)
}

// handleFeaturePrimary drives VAD from feature (RMS) as primary source.
// Returns true if barge-in was triggered.
func (s *Server) handleFeaturePrimary(st *sessionState, rms float64, now time.Time, sid string, stream gw.GatewayControl_SessionServer) bool {
	if !st.speaking {
		if now.Before(st.guardUntil) && rms >= st.minRMS {
			metricBargeInGuardBlocks.Inc()
			log.Printf("[orch] barge-in guard blocked sid=%s rms=%.1f minRMS=%.1f guard_remaining=%dms", sid, rms, st.minRMS, st.guardUntil.Sub(now).Milliseconds())
			return false
		}
		if rms >= st.minRMS {
			st.consecSpeech++
			if st.consecSpeech >= st.minStart {
				st.speaking = true
				st.nonSpeech = 0
				st.lastFeatureStart = now
				metricVADStarts.Inc()

				log.Printf("[orch] BARGE-IN TRIGGERED sid=%s rms=%.1f minRMS=%.1f consec=%d", sid, rms, st.minRMS, st.consecSpeech)

                // Barge-in: stop TTS
                s.sendCmd(stream, &gw.OrchestratorCommand{
                    SessionId: sid,
                    Cmd:       &gw.OrchestratorCommand_StopTts{StopTts: &gw.StopTTS{Reason: "barge_in"}},
                })
                metricBargeIn.Inc()
                metricBargeInTotal.Inc()

				// Cancel active LLM
				s.cancelLLM(st)

				// Record latency
				if !st.guardUntil.IsZero() && now.After(st.guardUntil) {
					metricBargeInLatency.Observe(float64(now.Sub(st.guardUntil).Milliseconds()))
				}

				// Log agreement with gateway VAD
				if !st.lastGatewayStart.IsZero() {
					d := now.Sub(st.lastGatewayStart)
					if d >= 0 {
						metricVADAgreeGatewayMS.Observe(float64(d.Milliseconds()))
						log.Printf("[orch] VAD agree: gateway %+dms relative to feature", d.Milliseconds())
					}
				}
				return true
			}
		} else {
			st.consecSpeech = 0
		}
		return false
	}

	// Currently speaking - check for end of speech
	if rms < st.minRMS {
		st.nonSpeech++
		if st.nonSpeech >= st.hangover {
			st.speaking = false
			st.consecSpeech = 0
			st.nonSpeech = 0
			st.lastFeatureStart = time.Time{} // Reset for next utterance
			st.lastGatewayStart = time.Time{}
			metricVADEnds.Inc()
		}
	} else {
		st.nonSpeech = 0
	}
	return false
}

// recordFeatureAgreement records feature VAD timing when gateway is primary.
func (s *Server) recordFeatureAgreement(st *sessionState, rms float64, now time.Time) {
	if rms >= st.minRMS && st.lastFeatureStart.IsZero() {
		st.lastFeatureStart = now
	}
}

// processGatewayVAD handles GatewayEvent_VadStart based on vadSource config.
// Returns true if barge-in was triggered.
func (s *Server) processGatewayVAD(st *sessionState, now time.Time, sid string, stream gw.GatewayControl_SessionServer) bool {
	st.lastGatewayStart = now

	if s.vadSource == "gateway" {
		// Primary: gateway drives VAD
		return s.handleGatewayVADPrimary(st, now, sid, stream)
	}

	// Secondary: just record agreement
	s.recordGatewayAgreement(st, now)
	return false
}

// handleGatewayVADPrimary drives VAD from gateway events as primary source.
// Returns true (always triggers barge-in when called as primary).
func (s *Server) handleGatewayVADPrimary(st *sessionState, now time.Time, sid string, stream gw.GatewayControl_SessionServer) bool {
    // Stop TTS
    s.sendCmd(stream, &gw.OrchestratorCommand{
        SessionId: sid,
        Cmd:       &gw.OrchestratorCommand_StopTts{StopTts: &gw.StopTTS{Reason: "barge_in"}},
    })
    metricBargeIn.Inc()
    metricBargeInTotal.Inc()

	// Cancel active LLM
	s.cancelLLM(st)

	// Log agreement with feature VAD
	if !st.lastFeatureStart.IsZero() {
		d := now.Sub(st.lastFeatureStart)
		if d >= 0 {
			metricVADAgreeFeatureMS.Observe(float64(d.Milliseconds()))
			log.Printf("[orch] VAD agree: feature %+dms relative to gateway", d.Milliseconds())
		}
	}
	return true
}

// recordGatewayAgreement records gateway VAD timing when feature is primary.
func (s *Server) recordGatewayAgreement(st *sessionState, now time.Time) {
	if !st.lastFeatureStart.IsZero() {
		d := now.Sub(st.lastFeatureStart)
		if d >= 0 {
			metricVADAgreeGatewayMS.Observe(float64(d.Milliseconds()))
			log.Printf("[orch] gateway VAD agreed %+dms after feature", d.Milliseconds())
		}
	}
}

// cancelLLM cancels any active LLM stream for the session.
func (s *Server) cancelLLM(st *sessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.llmActive && st.llmCancel != nil {
		st.llmCancel()
		st.llmActive = false
		st.llmCancel = nil
	}
}

// armBargeIn sets up the barge-in guard window for a session.
func (s *Server) armBargeIn(st *sessionState, guardMs uint32, minRms uint32) {
	st.minRMS = float64(minRms)
	st.armedAt = time.Now()
	st.guardUntil = st.armedAt.Add(time.Duration(guardMs) * time.Millisecond)
}

// resetVADState resets VAD counters (called when TTS starts).
func (s *Server) resetVADState(st *sessionState) {
	st.speaking = false
	st.consecSpeech = 0
	st.nonSpeech = 0
}
