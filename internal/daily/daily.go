package daily

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AudioConfig holds room/token audio settings for high-fidelity TTS
type AudioConfig struct {
	EnableMusicMode     bool
	AudioBitrate        int
	EnableDTX           bool
	EnablePrejoinUI     bool
	EnableNetworkUI     bool
	EnableNoiseCancelUI bool
}

type Client interface {
	CreateRoom(name, privacy string) error
	CreateMeetingToken(roomName, userName string, exp int64, isBot bool) (string, error)
}

type HTTPClient struct {
	http   *http.Client
	apiKey string
	base   string
	audio  AudioConfig
}

func NewClient(apiKey string, audio AudioConfig) *HTTPClient {
	return &HTTPClient{
		http:   &http.Client{Timeout: 10 * time.Second},
		apiKey: apiKey,
		base:   "https://api.daily.co/v1",
		audio:  audio,
	}
}

func (c *HTTPClient) CreateRoom(name, privacy string) error {
    // Build room properties with safe UI flags only.
    // Avoid unrecognized audio-specific fields at room level.
    properties := map[string]any{
        "enable_prejoin_ui":            c.audio.EnablePrejoinUI,
        "enable_network_ui":            c.audio.EnableNetworkUI,
        "enable_noise_cancellation_ui": c.audio.EnableNoiseCancelUI,
    }

    payload := map[string]any{
        "name":       name,
        "privacy":    privacy,
        "properties": properties,
    }
    resp, err := c.doJSONWithRetry("POST", c.base+"/rooms", payload)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusConflict { // 409 already exists
        io.Copy(io.Discard, resp.Body)
        return nil
    }
    if resp.StatusCode/100 != 2 {
        // On 400 invalid property, retry without properties
        if resp.StatusCode == http.StatusBadRequest {
            b, _ := io.ReadAll(resp.Body)
            payload2 := map[string]any{
                "name":    name,
                "privacy": privacy,
            }
            resp2, err2 := c.doJSONWithRetry("POST", c.base+"/rooms", payload2)
            if err2 != nil {
                return err2
            }
            defer resp2.Body.Close()
            if resp2.StatusCode == http.StatusConflict {
                io.Copy(io.Discard, resp2.Body)
                return nil
            }
            if resp2.StatusCode/100 != 2 {
                b2, _ := io.ReadAll(resp2.Body)
                return fmt.Errorf("daily CreateRoom: %s: %s (retry after %s: %s)", resp2.Status, string(b2), resp.Status, string(b))
            }
            return nil
        }
        b, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("daily CreateRoom: %s: %s", resp.Status, string(b))
    }
    return nil
}

func (c *HTTPClient) CreateMeetingToken(roomName, userName string, exp int64, isBot bool) (string, error) {
    properties := map[string]any{
        "room_name": roomName,
        "user_name": userName,
        "exp":       exp,
    }

    // Note: Daily meeting tokens do not support an 'enable_audio_processing' property.
    // Audio processing is disabled for the bot at the client level (customConstraints)
    // in gateway/main.py when enabling the virtual microphone.

	payload := map[string]any{
		"properties": properties,
	}
	resp, err := c.doJSONWithRetry("POST", c.base+"/meeting-tokens", payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("daily CreateMeetingToken: %s: %s", resp.Status, string(b))
	}
	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if parsed.Token == "" {
		return "", fmt.Errorf("daily CreateMeetingToken: empty token")
	}
	return parsed.Token, nil
}

// doJSONWithRetry creates a fresh request each attempt to avoid consumed bodies.
func (c *HTTPClient) doJSONWithRetry(method, url string, payload any) (*http.Response, error) {
	// two attempts max
	attempts := 0
	for {
		attempts++
		var buf bytes.Buffer
		if payload != nil {
			if err := json.NewEncoder(&buf).Encode(payload); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequest(method, url, &buf)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if attempts >= 2 {
				return nil, err
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			if attempts >= 2 {
				return resp, nil
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			time.Sleep(300 * time.Millisecond)
			continue
		}
		return resp, nil
	}
}
