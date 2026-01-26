package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"yuzu/agent/internal/config"
)

type CheckResult struct {
	Name    string        `json:"name"`
	OK      bool          `json:"ok"`
	Latency time.Duration `json:"latency_ms"`
	Error   string        `json:"error,omitempty"`
}

type HealthStatus struct {
	OK      bool          `json:"ok"`
	Checks  []CheckResult `json:"checks"`
	CheckedAt time.Time   `json:"checked_at"`
}

func (h HealthStatus) String() string {
	status := "OK"
	if !h.OK {
		status = "FAIL"
	}
	s := fmt.Sprintf("Health: %s\n", status)
	for _, c := range h.Checks {
		mark := "✓"
		if !c.OK {
			mark = "✗"
		}
		s += fmt.Sprintf("  %s %s (%dms)", mark, c.Name, c.Latency.Milliseconds())
		if c.Error != "" {
			s += fmt.Sprintf(" - %s", c.Error)
		}
		s += "\n"
	}
	return s
}

// CheckAll runs all health checks and returns combined status
func CheckAll(ctx context.Context, cfg config.Config) HealthStatus {
	checks := []CheckResult{
		checkDaily(ctx, cfg),
		checkElevenLabs(ctx, cfg),
	}

	allOK := true
	for _, c := range checks {
		if !c.OK {
			allOK = false
		}
	}

	return HealthStatus{
		OK:        allOK,
		Checks:    checks,
		CheckedAt: time.Now().UTC(),
	}
}

func checkDaily(ctx context.Context, cfg config.Config) CheckResult {
	start := time.Now()
	result := CheckResult{Name: "daily"}

	if cfg.Daily.APIKey == "" {
		result.Error = "DAILY_API_KEY not set"
		result.Latency = time.Since(start)
		return result
	}

	// Test Daily API by listing rooms (lightweight call)
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.daily.co/v1/rooms?limit=1", nil)
	if err != nil {
		result.Error = fmt.Sprintf("request build failed: %v", err)
		result.Latency = time.Since(start)
		return result
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Daily.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.Error = fmt.Sprintf("request failed: %v", err)
		result.Latency = time.Since(start)
		return result
	}
	defer resp.Body.Close()

	result.Latency = time.Since(start)

	if resp.StatusCode == 401 {
		result.Error = "invalid API key (401)"
		return result
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		result.Error = fmt.Sprintf("unexpected status %d: %s", resp.StatusCode, string(body))
		return result
	}

	result.OK = true
	return result
}

func checkElevenLabs(ctx context.Context, cfg config.Config) CheckResult {
	start := time.Now()
	result := CheckResult{Name: "elevenlabs"}

	if cfg.Eleven.APIKey == "" {
		result.Error = "ELEVENLABS_API_KEY not set"
		result.Latency = time.Since(start)
		return result
	}

	if cfg.Eleven.VoiceID == "" {
		result.Error = "ELEVENLABS_VOICE_ID not set"
		result.Latency = time.Since(start)
		return result
	}

	// Test ElevenLabs by making a minimal TTS request (1 character)
	// This works with TTS-only API keys that lack user_read permission
	url := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s/stream", cfg.Eleven.VoiceID)
	body := `{"text":"."}`
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	if err != nil {
		result.Error = fmt.Sprintf("request build failed: %v", err)
		result.Latency = time.Since(start)
		return result
	}
	req.Header.Set("xi-api-key", cfg.Eleven.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.Error = fmt.Sprintf("request failed: %v", err)
		result.Latency = time.Since(start)
		return result
	}
	defer resp.Body.Close()

	result.Latency = time.Since(start)

	if resp.StatusCode == 401 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		result.Error = fmt.Sprintf("invalid API key (401): %s", string(bodyBytes))
		return result
	}
	if resp.StatusCode == 404 {
		result.Error = fmt.Sprintf("voice ID %q not found", cfg.Eleven.VoiceID)
		return result
	}
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		result.Error = fmt.Sprintf("unexpected status %d: %s", resp.StatusCode, string(bodyBytes))
		return result
	}

	// Drain response body (we don't need the audio)
	io.Copy(io.Discard, resp.Body)

	result.OK = true
	return result
}

// CheckVoiceID verifies a specific voice ID exists (optional, more expensive check)
func CheckVoiceID(ctx context.Context, cfg config.Config) CheckResult {
	start := time.Now()
	result := CheckResult{Name: "elevenlabs_voice"}

	if cfg.Eleven.APIKey == "" || cfg.Eleven.VoiceID == "" {
		result.Error = "ELEVENLABS_API_KEY or ELEVENLABS_VOICE_ID not set"
		result.Latency = time.Since(start)
		return result
	}

	url := fmt.Sprintf("https://api.elevenlabs.io/v1/voices/%s", cfg.Eleven.VoiceID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		result.Error = fmt.Sprintf("request build failed: %v", err)
		result.Latency = time.Since(start)
		return result
	}
	req.Header.Set("xi-api-key", cfg.Eleven.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.Error = fmt.Sprintf("request failed: %v", err)
		result.Latency = time.Since(start)
		return result
	}
	defer resp.Body.Close()

	result.Latency = time.Since(start)

	if resp.StatusCode == 404 {
		result.Error = fmt.Sprintf("voice ID %q not found", cfg.Eleven.VoiceID)
		return result
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		result.Error = fmt.Sprintf("unexpected status %d: %s", resp.StatusCode, string(body))
		return result
	}

	result.OK = true
	return result
}
