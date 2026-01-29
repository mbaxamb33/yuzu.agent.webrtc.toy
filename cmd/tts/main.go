package main

import (
    "flag"
    "log"
    "net"
    "net/http"

    "google.golang.org/grpc"

    tts "yuzu/agent/internal/tts"
    pb "yuzu/agent/internal/tts/pb"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

var addr = flag.String("addr", ":9093", "tts service listen addr")

func main(){
    flag.Parse()
    s := grpc.NewServer()
    srv := tts.NewServer()
    pb.RegisterTTSServer(s, srv)

    go func(){
        mux := http.NewServeMux()
        mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
        mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
        mux.Handle("/metrics", promhttp.Handler())
        log.Printf("tts probes/metrics on :8084")
        _ = http.ListenAndServe(":8084", mux)
    }()

    l, err := net.Listen("tcp", *addr)
    if err != nil { log.Fatalf("listen: %v", err) }
    log.Printf("tts listening on %s", *addr)
    if err := s.Serve(l); err != nil { log.Fatalf("serve: %v", err) }
}

