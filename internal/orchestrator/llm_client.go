package orchestrator

import (
    "context"
    "math/rand"
    "os"
    "time"

    llmpb "yuzu/agent/internal/llm/pb"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

// getLLMClient returns a persistent LLM client, lazily initialized.
func (s *Server) getLLMClient(ctx context.Context) (llmpb.LLMClient, error) {
    s.llmMu.RLock()
    if s.llmClient != nil {
        defer s.llmMu.RUnlock()
        return s.llmClient, nil
    }
    s.llmMu.RUnlock()

    // Acquire write lock and recheck (double-checked locking)
    s.llmMu.Lock()
    defer s.llmMu.Unlock()
    if s.llmClient != nil {
        return s.llmClient, nil
    }

    addr := os.Getenv("LLM_ADDR")
    if addr == "" { addr = ":9092" }
    conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
    if err != nil { return nil, err }
    client := llmpb.NewLLMClient(conn)
    s.llmConn = conn
    s.llmClient = client
    return client, nil
}

// reconnectLLM closes the existing connection and re-dials with exponential backoff.
func (s *Server) reconnectLLM(ctx context.Context, attempt int) error {
    s.llmMu.Lock()
    if s.llmConn != nil {
        _ = s.llmConn.Close()
        s.llmConn = nil
        s.llmClient = nil
    }
    s.llmMu.Unlock()

    // Backoff: base 200ms, capped, with jitter
    base := 200 * time.Millisecond
    pow := 1 << uint(min(attempt, 5)) // 1,2,4,8,16, capped
    sleep := time.Duration(pow) * base
    jitter := time.Duration(rand.Int63n(int64(base)))
    timer := time.NewTimer(sleep + jitter)
    defer timer.Stop()
    select {
    case <-timer.C:
    case <-ctx.Done():
        return ctx.Err()
    }
    _, err := s.getLLMClient(ctx)
    if err == nil {
        metricLLMReconnects.Inc()
    }
    return err
}

func min(a, b int) int { if a < b { return a }; return b }
