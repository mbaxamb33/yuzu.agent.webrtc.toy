package main

import (
    "context"
    "flag"
    "log"
    "net"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "google.golang.org/grpc"
    "google.golang.org/grpc/keepalive"

    "github.com/prometheus/client_golang/prometheus/promhttp"

    pb "yuzu/agent/internal/stt/pb"
    sttsrv "yuzu/agent/internal/stt"
)

// UDS default location; override with --uds or STT_UDS_PATH
var (
    udsPath   = flag.String("uds", "", "unix domain socket path (default /run/app/stt.sock)")
    httpProbe = flag.String("http", ":8081", "http addr for health/ready probes")
)

func main() {
    flag.Parse()
    path := *udsPath
    if path == "" {
        path = os.Getenv("STT_UDS_PATH")
        if path == "" {
            path = "/run/app/stt.sock"
        }
    }
    _ = os.Remove(path)
    if err := os.MkdirAll(dir(path), 0755); err != nil {
        log.Fatalf("mkdir %s: %v", dir(path), err)
    }
    l, err := net.Listen("unix", path)
    if err != nil {
        log.Fatalf("listen unix %s: %v", path, err)
    }
    if err := os.Chmod(path, 0770); err != nil {
        log.Printf("chmod uds: %v", err)
    }

    // gRPC server with keepalive for fast death detection
    kap := keepalive.ServerParameters{
        MaxConnectionIdle:     2 * time.Minute,
        MaxConnectionAge:      15 * time.Minute,
        MaxConnectionAgeGrace: 30 * time.Second,
        Time:                  30 * time.Second,
        Timeout:               10 * time.Second,
    }
    kasp := keepalive.EnforcementPolicy{
        MinTime:             10 * time.Second,
        PermitWithoutStream: true,
    }

    s := grpc.NewServer(grpc.KeepaliveParams(kap), grpc.KeepaliveEnforcementPolicy(kasp))
    srv := sttsrv.NewSTTServer()
    pb.RegisterSTTServer(s, srv)

    // Health/ready probes
    go func() {
        mux := http.NewServeMux()
        mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
        mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
            if srv.Ready() {
                w.Write([]byte("ok\n"))
                return
            }
            w.WriteHeader(503)
            w.Write([]byte("not ready\n"))
        })
        mux.Handle("/metrics", promhttp.Handler())
        log.Printf("probes/metrics on %s", *httpProbe)
        _ = http.ListenAndServe(*httpProbe, mux)
    }()

    log.Printf("STT sidecar listening on UDS %s", path)

    // Graceful shutdown handler
    stopCh := make(chan os.Signal, 1)
    signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
    go func() {
        <-stopCh
        log.Printf("shutdown signal received, draining...")
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = srv.GracefulShutdown(ctx, 5*time.Second)
        s.GracefulStop()
    }()

    if err := s.Serve(l); err != nil {
        log.Fatalf("grpc serve: %v", err)
    }
}

func dir(p string) string {
    i := len(p) - 1
    for i >= 0 && p[i] != '/' { i-- }
    if i <= 0 { return "." }
    return p[:i]
}

