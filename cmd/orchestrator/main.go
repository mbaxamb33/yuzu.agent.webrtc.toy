package main

import (
    "flag"
    "log"
    "net"
    "net/http"

    "google.golang.org/grpc"

    orch "yuzu/agent/internal/orchestrator"
    gw "yuzu/agent/internal/orchestrator/pb"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
    addr = flag.String("addr", ":9090", "orchestrator listen addr")
)

func main(){
    flag.Parse()
    s := grpc.NewServer()
    srv := orch.NewServer()
    gw.RegisterGatewayControlServer(s, srv)

    // health endpoints
    go func(){
        mux := http.NewServeMux()
        mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
        mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
        mux.Handle("/metrics", promhttp.Handler())
        log.Printf("orchestrator probes/metrics on :8082")
        _ = http.ListenAndServe(":8082", mux)
    }()

    l, err := net.Listen("tcp", *addr)
    if err != nil { log.Fatalf("listen: %v", err) }
    log.Printf("orchestrator listening on %s", *addr)
    if err := s.Serve(l); err != nil { log.Fatalf("serve: %v", err) }
}
