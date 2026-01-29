package llm

import (
    "bufio"
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "strings"
    "time"

    pb "yuzu/agent/internal/llm/pb"
)

type Server struct {
    pb.UnimplementedLLMServer
    httpc *http.Client
}

func NewServer() *Server {
    return &Server{httpc: &http.Client{Timeout: 0}}
}

func (s *Server) Session(stream pb.LLM_SessionServer) error {
    parent := stream.Context()
    // Expect a StartRequest; support Cancel thereafter
    msg, err := stream.Recv()
    if err != nil { return err }
    start := msg.GetStart()
    if start == nil { return fmt.Errorf("expected start request") }
    _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Connected{Connected: &pb.Connected{SessionId: start.GetSessionId()}}})

    azureEndpoint := os.Getenv("AZURE_OPENAI_ENDPOINT")
    apiKey := os.Getenv("AZURE_OPENAI_API_KEY")
    if azureEndpoint == "" || apiKey == "" {
        _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Error{Error: &pb.Error{Code: "config", Message: "missing AZURE_OPENAI_ENDPOINT or AZURE_OPENAI_API_KEY"}}})
        return nil
    }

    deployment := start.GetDeployment()
    apiVersion := start.GetApiVersion()
    if apiVersion == "" { apiVersion = "2024-02-15-preview" }

    // Build Azure requests body
    body := map[string]any{
        "stream": true,
        "messages": toAzureMessages(start.GetMessages()),
    }
    if start.GetMaxTokens() > 0 { body["max_tokens"] = start.GetMaxTokens() }
    if start.GetTemperature() > 0 { body["temperature"] = start.GetTemperature() }

    url := fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s", strings.TrimRight(azureEndpoint, "/"), deployment, apiVersion)
    reqBytes, _ := json.Marshal(body)
    // Derive a cancellable context we can cancel on Client Cancel message
    ctx, cancel := context.WithCancel(parent)
    defer cancel()
    // Concurrently listen for Cancel messages
    go func(){
        for {
            cm, err := stream.Recv()
            if err != nil { return }
            if c := cm.GetCancel(); c != nil {
                cancel()
                return
            }
        }
    }()

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBytes))
    if err != nil { return err }
    req.Header.Set("api-key", apiKey)
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Accept", "text/event-stream")
    // Azure streams as text/event-stream
    resp, err := s.httpc.Do(req)
    if err != nil { return err }
    defer resp.Body.Close()
    if resp.StatusCode/100 != 2 {
        b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
        _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Error{Error: &pb.Error{Code: "http", Message: fmt.Sprintf("status=%d body=%s", resp.StatusCode, string(b))}}})
        return nil
    }

    br := bufio.NewReader(resp.Body)
    startTime := time.Now()
    firstTokenSent := false
    var sentBuf bytes.Buffer
    decoder := newSSEDecoder(br)
    for {
        if ctx.Err() != nil { return nil }
        event, data, err := decoder.Next()
        if err != nil {
            if err == io.EOF { break }
            // non-fatal: send error and break
            _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Error{Error: &pb.Error{Code: "stream", Message: err.Error()}}})
            break
        }
        if event == "" && len(data) == 0 { continue }
        if string(data) == "[DONE]" { break }
        // Parse Azure chunk
        var m map[string]any
        if err := json.Unmarshal(data, &m); err != nil { continue }
        choices, _ := m["choices"].([]any)
        if len(choices) == 0 { continue }
        choice, _ := choices[0].(map[string]any)
        delta, _ := choice["delta"].(map[string]any)
        content := toString(delta["content"])
        if content != "" {
            if !firstTokenSent {
                ttft := time.Since(startTime).Milliseconds()
                // Could export Prometheus here if desired
                _ = ttft
                firstTokenSent = true
            }
            sentBuf.WriteString(content)
            _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Token{Token: &pb.Token{Text: content}}})
            // sentence segmentation
            if isSentenceBoundary(sentBuf.String()) {
                sentence := sentBuf.String()
                _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Sentence{Sentence: &pb.Sentence{Text: sentence}}})
                sentBuf.Reset()
            }
        }
        // usage in final payload
        if usage, ok := m["usage"].(map[string]any); ok {
            pt := toInt(usage["prompt_tokens"]) ; ct := toInt(usage["completion_tokens"]) ; tt := toInt(usage["total_tokens"]) 
            _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Usage{Usage: &pb.Usage{PromptTokens: uint32(pt), CompletionTokens: uint32(ct), TotalTokens: uint32(tt)}}})
        }
    }
    // Flush any trailing partial sentence
    if sentBuf.Len() > 0 {
        _ = stream.Send(&pb.ServerMessage{Msg: &pb.ServerMessage_Sentence{Sentence: &pb.Sentence{Text: sentBuf.String()}}})
    }
    return nil
}

func toAzureMessages(in []*pb.ChatMessage) []map[string]any {
    out := make([]map[string]any, 0, len(in))
    for _, m := range in {
        out = append(out, map[string]any{"role": m.GetRole(), "content": m.GetContent()})
    }
    return out
}

type sseDecoder struct {
    r *bufio.Reader
}

func newSSEDecoder(r *bufio.Reader) *sseDecoder { return &sseDecoder{r: r} }

// Next returns (event, data, error). For Azure, event is often empty; data lines begin with "data: ".
func (d *sseDecoder) Next() (string, []byte, error) {
    var event string
    var data []byte
    for {
        line, err := d.r.ReadBytes('\n')
        if err != nil { return "", nil, err }
        line = bytes.TrimSpace(line)
        if len(line) == 0 { // dispatch
            if len(data) == 0 { continue }
            return event, data, nil
        }
        if bytes.HasPrefix(line, []byte("event:")) {
            event = strings.TrimSpace(string(line[len("event:"):]))
        } else if bytes.HasPrefix(line, []byte("data:")) {
            data = append(data, bytes.TrimSpace(line[len("data:"):])...)
        }
    }
}

func isSentenceBoundary(s string) bool {
    // naive boundary: period, exclamation, question
    // ensure trailing whitespace/newline is allowed
    t := strings.TrimSpace(s)
    if t == "" { return false }
    last := t[len(t)-1]
    return last == '.' || last == '!' || last == '?'
}

func toString(v any) string { if v==nil { return "" }; if s,ok:=v.(string); ok { return s }; return "" }
func toInt(v any) int {
    switch t := v.(type) {
    case float64: return int(t)
    case int: return t
    default: return 0
    }
}
