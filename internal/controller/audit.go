package controller

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type responseStatus struct {
	http.ResponseWriter
	status int
}

func (w *responseStatus) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseStatus) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *responseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Record mutation metadata only, never request bodies, terminal input, URLs with
// query strings, cookies, passwords, or provider response bodies.
func (s *Server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		id := randomID()[:16]
		w.Header().Set("X-Request-ID", id)
		status := &responseStatus{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(status, r)
		code := status.status
		if code == 0 {
			code = 200
		}
		event := struct {
			Time       time.Time `json:"time"`
			ID         string    `json:"requestId"`
			Method     string    `json:"method"`
			Path       string    `json:"path"`
			Status     int       `json:"status"`
			DurationMS int64     `json:"durationMs"`
		}{time.Now().UTC(), id, r.Method, r.URL.EscapedPath(), code, time.Since(start).Milliseconds()}
		data, err := json.Marshal(event)
		if err == nil {
			err = s.appendAudit(append(data, '\n'))
		}
		if err != nil {
			log.Print("Could not write mutation audit record: ", err)
		}
	})
}

func (s *Server) appendAudit(data []byte) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	path := filepath.Join(s.config.StateDir, "audit.jsonl")
	if info, err := os.Stat(path); err == nil && info.Size() > 8<<20 {
		if err = os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
