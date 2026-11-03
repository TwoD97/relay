package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TwoD97/relay/internal/sessionimage"
	"github.com/TwoD97/relay/internal/transport"
)

type imageSession struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Status    string    `json:"status"`
	Purpose   string    `json:"purpose,omitempty"`
}

func validateImageSession(ctx context.Context, client *http.Client, id string, created time.Time) error {
	data, err := runtimeJSON(ctx, client, "GET", "/api/sessions", nil)
	if err != nil {
		return err
	}
	var sessions []imageSession
	if json.Unmarshal(data, &sessions) != nil {
		return errors.New("session list could not be verified")
	}
	found := false
	for _, session := range sessions {
		if session.ID != id {
			continue
		}
		if found || !session.CreatedAt.Equal(created) || session.Status != "running" || session.Purpose != "" {
			return errors.New("the selected session changed or is no longer an active terminal")
		}
		found = true
	}
	if !found {
		return errors.New("the selected session no longer exists")
	}
	return nil
}
func (s *Server) uploadImage(w http.ResponseWriter, r *http.Request) {
	id, sessionID := r.PathValue("id"), r.PathValue("session")
	decoded, err := hex.DecodeString(sessionID)
	if err != nil || len(decoded) != 16 || sessionID != strings.ToLower(sessionID) {
		writeError(w, 400, "Invalid session identity")
		return
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) != 1 || len(values["createdAt"]) != 1 || len(values.Get("createdAt")) > 64 {
		writeError(w, 400, "The exact session creation time is required")
		return
	}
	created, err := time.Parse(time.RFC3339Nano, values.Get("createdAt"))
	if err != nil || created.IsZero() {
		writeError(w, 400, "Invalid session creation time")
		return
	}
	key, _ := sessionimage.Key(sessionID, created)
	select {
	case s.imageSlots <- struct{}{}:
		defer func() { <-s.imageSlots }()
	default:
		writeError(w, 429, "Other images are being transferred. Try again shortly.")
		return
	}
	s.mu.Lock()
	l := s.links[id]
	host, exists := s.store.get(id)
	local := id == "local" && s.hasLocalMastermind()
	if local {
		exists = true
		host.Status = "online"
	}
	var client *http.Client
	var connection *transport.Connection
	if exists && host.Status == "online" && l != nil && l.httpTransport != nil && (local || l.conn != nil && l.conn.Ready()) {
		client = &http.Client{Transport: l.httpTransport}
		connection = l.conn
	}
	s.mu.Unlock()
	if client == nil {
		writeError(w, 503, "Reconnect to this machine before pasting an image")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	if err = validateImageSession(ctx, client, sessionID, created); err != nil {
		writeError(w, 409, "The selected session could not be verified. Refresh it before pasting an image.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, sessionimage.MaxBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var size *http.MaxBytesError
		if errors.As(err, &size) {
			writeError(w, 413, "Images must be at most 10 MiB")
		} else {
			writeError(w, 400, "Image data could not be read")
		}
		return
	}
	image, err := sessionimage.Validate(data, r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err = ctx.Err(); err != nil {
		writeError(w, 408, "Image transfer was cancelled before saving")
		return
	}
	var path string
	if local {
		path, err = sessionimage.Store(ctx, s.config.StateDir, key, image)
	} else {
		path, err = connection.UploadSessionImage(ctx, key, image)
	}
	if err != nil {
		if errors.Is(err, transport.ErrImageTools) {
			writeError(w, 503, err.Error())
		} else if errors.Is(err, sessionimage.ErrQuota) {
			writeError(w, 507, err.Error())
		} else {
			writeError(w, 502, "Image transfer could not be confirmed. No terminal input was sent and the upload was not retried.")
		}
		return
	}
	s.mu.Lock()
	same := s.links[id] == l
	s.mu.Unlock()
	if !same || validateImageSession(ctx, client, sessionID, created) != nil {
		writeError(w, 409, "The image was saved, but the session or connection changed. No terminal input was sent.")
		return
	}
	writeJSON(w, 201, map[string]any{"path": path, "mime": image.MIME(), "bytes": image.Size(), "sha256": image.SHA256()})
}
