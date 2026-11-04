//go:build linux

package runtime

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/TwoD97/relay/internal/peers"
)

func (s *Server) initializePeers() {
	s.peerStore, s.peerError = peers.Open(filepath.Join(s.stateDir, "peers"))
}
func (s *Server) registerPeers(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/peers", s.listPeers)
	mux.HandleFunc("GET /api/peers/{source}", s.getPeer)
	mux.HandleFunc("PUT /api/peers/{source}", s.putPeer)
	mux.HandleFunc("DELETE /api/peers/{source}", s.deletePeer)
}
func (s *Server) peersReady(w http.ResponseWriter) bool {
	if s.peerStore == nil || s.peerError != nil {
		fail(w, 500, errors.New("Peer storage is unavailable; existing snapshots were preserved"))
		return false
	}
	return true
}
func peerFailure(w http.ResponseWriter, err error) {
	status := 500
	switch {
	case errors.Is(err, peers.ErrInvalid):
		status = 400
	case errors.Is(err, peers.ErrConflict):
		status = 409
	case errors.Is(err, peers.ErrNotFound):
		status = 404
	case errors.Is(err, peers.ErrCapacity):
		status = 507
	case errors.Is(err, peers.ErrBusy):
		status = 503
	}
	if status == 500 && !errors.Is(err, peers.ErrCorrupt) {
		err = errors.New("Peer storage operation could not be confirmed; read the source before another attempt")
	}
	fail(w, status, err)
}
func (s *Server) listPeers(w http.ResponseWriter, r *http.Request) {
	if !s.peersReady(w) {
		return
	}
	value, err := s.peerStore.List()
	if err != nil {
		peerFailure(w, err)
		return
	}
	jsonResponse(w, 200, value)
}
func (s *Server) getPeer(w http.ResponseWriter, r *http.Request) {
	if !s.peersReady(w) {
		return
	}
	value, err := s.peerStore.Get(r.PathValue("source"))
	if err != nil {
		peerFailure(w, err)
		return
	}
	jsonResponse(w, 200, value)
}
func decodePeer(w http.ResponseWriter, r *http.Request, value any) error {
	if r.Context().Err() != nil {
		return r.Context().Err()
	}
	r.Body = http.MaxBytesReader(w, r.Body, peers.MaxBytes+1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected one peer request")
	}
	return nil
}
func (s *Server) putPeer(w http.ResponseWriter, r *http.Request) {
	if !s.peersReady(w) {
		return
	}
	var body struct {
		ExpectedDigest string         `json:"expectedDigest"`
		Snapshot       peers.Snapshot `json:"snapshot"`
	}
	if decodePeer(w, r, &body) != nil || body.Snapshot.SourceID != r.PathValue("source") {
		fail(w, 400, peers.ErrInvalid)
		return
	}
	value, err := s.peerStore.Put(body.ExpectedDigest, body.Snapshot)
	if err != nil {
		peerFailure(w, err)
		return
	}
	jsonResponse(w, 200, value)
}
func (s *Server) deletePeer(w http.ResponseWriter, r *http.Request) {
	if !s.peersReady(w) {
		return
	}
	var body struct {
		ExpectedDigest string `json:"expectedDigest"`
	}
	if decodePeer(w, r, &body) != nil {
		fail(w, 400, peers.ErrInvalid)
		return
	}
	if err := s.peerStore.Delete(r.PathValue("source"), body.ExpectedDigest); err != nil {
		peerFailure(w, err)
		return
	}
	w.WriteHeader(204)
}
