package main

import (
    "flag"
    "log"
    "net"
    "net/http"

    "google.golang.org/grpc"

    llm "yuzu/agent/internal/llm"
    pb "yuzu/agent/internal/llm/pb"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
    addr = flag.String("addr", ":9092", "llm service listen addr")
)

func main(){
    flag.Parse()
    s := grpc.NewServer()
    srv := llm.NewServer()
    pb.RegisterLLMServer(s, srv)

    // metrics/health
    go func(){
        mux := http.NewServeMux()
        mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
        mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
        mux.Handle("/metrics", promhttp.Handler())
        log.Printf("llm probes/metrics on :8083")
        _ = http.ListenAndServe(":8083", mux)
    }()

    l, err := net.Listen("tcp", *addr)
    if err != nil { log.Fatalf("listen: %v", err) }
    log.Printf("llm listening on %s", *addr)
    if err := s.Serve(l); err != nil { log.Fatalf("serve: %v", err) }
}

