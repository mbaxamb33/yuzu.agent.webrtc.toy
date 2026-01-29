package orchestrator

import (
    "context"
    "os"

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
