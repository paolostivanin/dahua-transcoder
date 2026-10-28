package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

// API:
//
//	GET    /healthz              liveness, no token (still IP-filtered)
//	GET    /v1/status            version, hwaccel, active job count
//	POST   /v1/jobs              {"channel":1,"start":<unix>,"duration":210}
//	GET    /v1/jobs/{id}         job state and timings
//	GET    /v1/jobs/{id}/video   the MP4 once ready (byte ranges supported)
//	DELETE /v1/jobs/{id}         cancel, or free a finished job
//
// Every /v1 call needs "Authorization: Bearer <token>".
type api struct {
	m         *Manager
	tokenHash [32]byte
}

func newAPI(m *Manager) http.Handler {
	a := &api{m: m, tokenHash: sha256.Sum256([]byte(m.cfg.Token))}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})
	mux.HandleFunc("GET /v1/status", a.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.m.Status())
	}))
	mux.HandleFunc("POST /v1/jobs", a.auth(a.create))
	mux.HandleFunc("GET /v1/jobs/{id}", a.auth(a.get))
	mux.HandleFunc("GET /v1/jobs/{id}/video", a.auth(a.video))
	mux.HandleFunc("DELETE /v1/jobs/{id}", a.auth(a.remove))
	return a.ipFilter(mux)
}

func (a *api) ipFilter(next http.Handler) http.Handler {
	allowed := a.m.cfg.allowed
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(allowed) > 0 {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			addr, perr := netip.ParseAddr(host)
			ok := false
			if err == nil && perr == nil {
				addr = addr.Unmap()
				for _, p := range allowed {
					if p.Contains(addr) {
						ok = true
						break
					}
				}
			}
			if !ok {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *api) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare(sum[:], a.tokenHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dahua-transcoder"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func (a *api) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel  int   `json:"channel"`
		Start    int64 `json:"start"`
		Duration int   `json:"duration"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	job, err := a.m.Submit(req.Channel, req.Start, req.Duration)
	var inv *invalidError
	var notYet *notYetError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, job)
	case errors.As(err, &inv):
		writeError(w, http.StatusBadRequest, inv.msg)
	case errors.As(err, &notYet):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": notYet.Error(), "state": "waiting", "available_at": notYet.availableAt,
		})
	case errors.Is(err, errQueueFull):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, errShuttingDown):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (a *api) get(w http.ResponseWriter, r *http.Request) {
	job, err := a.m.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (a *api) video(w http.ResponseWriter, r *http.Request) {
	path, job, err := a.m.Video(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if job.State != StateReady || path == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "video not ready", "state": job.State})
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "video no longer available")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (a *api) remove(w http.ResponseWriter, r *http.Request) {
	if err := a.m.Delete(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
