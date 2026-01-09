package api

import (
	"net/http"
	"strings"
)

func NewRouter(h *Handlers) http.Handler {
    mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			h.HandleCreateSession(w, r)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})

    mux.HandleFunc("/sessions/", func(w http.ResponseWriter, r *http.Request) {
		// /sessions/{id}/start | /end | /events
		path := strings.TrimSuffix(r.URL.Path, "/")
		const prefix = "/sessions/"
		if !strings.HasPrefix(path, prefix) {
			http.NotFound(w, r)
			return
		}
		rest := strings.TrimPrefix(path, prefix)
		parts := strings.Split(rest, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		id := parts[0]
		tail := ""
		if len(parts) > 1 {
			tail = parts[1]
		}

        switch tail {
        case "start":
            if r.Method != http.MethodPost {
                http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
                return
            }
            h.HandleStartSession(w, r, id)
            return
        case "end":
            if r.Method != http.MethodPost {
                http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
                return
            }
            h.HandleEndSession(w, r, id)
            return
        case "events":
            if r.Method != http.MethodGet {
                http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
                return
            }
            h.HandleListEvents(w, r, id)
            return
        case "worker-token":
            if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
            h.HandleMintWorkerToken(w, r, id)
            return
        case "ws-creds":
            if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
            h.HandleMintWSCreds(w, r, id)
            return
        case "debug":
            if len(parts) < 3 { http.NotFound(w, r); return }
            action := parts[2]
            if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
            switch action {
            case "vad-start":
                h.HandleDebugVAD(w, r, id, "vad_start")
                return
            case "vad-end":
                h.HandleDebugVAD(w, r, id, "vad_end")
                return
            default:
                http.NotFound(w, r)
                return
            }
        default:
            http.NotFound(w, r)
            return
        }
    })

    return mux
}
