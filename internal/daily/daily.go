package daily

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"
)

type Client interface {
    CreateRoom(name, privacy string) error
    CreateMeetingToken(roomName, userName string, exp int64) (string, error)
}

type HTTPClient struct {
    http  *http.Client
    apiKey string
    base  string
}

func NewClient(apiKey string) *HTTPClient {
    return &HTTPClient{
        http:  &http.Client{Timeout: 10 *  time.Second},
        apiKey: apiKey,
        base:  "https://api.daily.co/v1",
    }
}

func (c *HTTPClient) CreateRoom(name, privacy string) error {
    body := map[string]any{
        "name":    name,
        "privacy": privacy,
    }
    var out bytes.Buffer
    if err := json.NewEncoder(&out).Encode(body); err != nil {
        return err
    }
    req, err := http.NewRequest("POST", c.base+"/rooms", &out)
    if err != nil { return err }
    req.Header.Set("Authorization", "Bearer "+c.apiKey)
    req.Header.Set("Content-Type", "application/json")
    resp, err := c.doWithRetry(req)
    if err != nil { return err }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusConflict { // 409 already exists
        io.Copy(io.Discard, resp.Body)
        return nil
    }
    if resp.StatusCode/100 != 2 {
        b, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("daily CreateRoom: %s: %s", resp.Status, string(b))
    }
    return nil
}

func (c *HTTPClient) CreateMeetingToken(roomName, userName string, exp int64) (string, error) {
    body := map[string]any{
        "properties": map[string]any{
            "room_name": roomName,
            "user_name": userName,
            "exp":       exp,
        },
    }
    var out bytes.Buffer
    if err := json.NewEncoder(&out).Encode(body); err != nil {
        return "", err
    }
    req, err := http.NewRequest("POST", c.base+"/meeting-tokens", &out)
    if err != nil { return "", err }
    req.Header.Set("Authorization", "Bearer "+c.apiKey)
    req.Header.Set("Content-Type", "application/json")
    resp, err := c.doWithRetry(req)
    if err != nil { return "", err }
    defer resp.Body.Close()
    if resp.StatusCode/100 != 2 {
        b, _ := io.ReadAll(resp.Body)
        return "", fmt.Errorf("daily CreateMeetingToken: %s: %s", resp.Status, string(b))
    }
    var parsed struct{ Token string `json:"token"` }
    if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
        return "", err
    }
    if parsed.Token == "" {
        return "", fmt.Errorf("daily CreateMeetingToken: empty token")
    }
    return parsed.Token, nil
}

// doWithRetry retries once on 429/5xx.
func (c *HTTPClient) doWithRetry(req *http.Request) (*http.Response, error) {
    resp, err := c.http.Do(req)
    if err != nil {
        return nil, err
    }
    if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
        // simple backoff
        io.Copy(io.Discard, resp.Body)
        resp.Body.Close()
        time.Sleep(300 * time.Millisecond)
        return c.http.Do(req)
    }
    return resp, nil
}
