package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TwoD97/relay/internal/skillmanager"
)

func (s *Server) RegisterSkillRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/skills", s.listSkills)
	mux.HandleFunc("POST /api/skills/inspect", s.inspectSkill)
	mux.HandleFunc("POST /api/skills/import", s.importSkill)
	mux.HandleFunc("GET /api/skills/{name}/export", s.exportSkill)
	mux.HandleFunc("DELETE /api/skills/{name}", s.removeSkill)
	mux.HandleFunc("GET /api/hosts/{id}/skills", s.hostSkills)
	mux.HandleFunc("POST /api/hosts/{id}/skills/check", s.hostSkills)
	mux.HandleFunc("POST /api/hosts/{id}/skills/{name}/deploy", s.hostSkills)
	mux.HandleFunc("POST /api/hosts/{id}/skills/{name}/reconcile", s.hostSkills)
	mux.HandleFunc("DELETE /api/hosts/{id}/skills/{name}", s.hostSkills)
}
func (s *Server) skillLibrary() string { return filepath.Join(s.config.StateDir, "skills") }
func skillError(w http.ResponseWriter, err error) {
	status := 400
	if errors.Is(err, skillmanager.ErrConflict) {
		status = 409
	}
	if errors.Is(err, os.ErrNotExist) {
		status = 404
	}
	writeError(w, status, err.Error())
}
func (s *Server) listSkills(w http.ResponseWriter, _ *http.Request) {
	v, e := skillmanager.List(s.skillLibrary())
	if e != nil {
		writeError(w, 503, "Skills library unavailable; existing files preserved")
		return
	}
	writeJSON(w, 200, v)
}
func readSkillImport(w http.ResponseWriter, r *http.Request) (skillmanager.Package, error) {
	typ := strings.Split(r.Header.Get("Content-Type"), ";")[0]
	if typ != "application/zip" && typ != "text/markdown" {
		return skillmanager.Package{}, errors.New("Import a ZIP skill folder or a Markdown SKILL.md file")
	}
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, skillmanager.MaxBytes))
	if e != nil {
		return skillmanager.Package{}, errors.New("Skill import exceeds 4 MiB")
	}
	return skillmanager.Parse(b, typ == "application/zip")
}
func (s *Server) inspectSkill(w http.ResponseWriter, r *http.Request) {
	p, e := readSkillImport(w, r)
	if e != nil {
		skillError(w, e)
		return
	}
	paths := []string{}
	for _, f := range p.Files {
		paths = append(paths, f.Path)
	}
	writeJSON(w, 200, struct {
		skillmanager.Entry
		Paths []string `json:"paths"`
	}{p.Entry(), paths})
}
func (s *Server) importSkill(w http.ResponseWriter, r *http.Request) {
	revision, e := strconv.ParseUint(r.URL.Query().Get("revision"), 10, 53)
	if e != nil {
		writeError(w, 400, "A current library revision is required")
		return
	}
	p, e := readSkillImport(w, r)
	if e != nil {
		skillError(w, e)
		return
	}
	v, e := skillmanager.Import(s.skillLibrary(), revision, r.URL.Query().Get("expectedDigest"), p)
	if e != nil {
		skillError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) exportSkill(w http.ResponseWriter, r *http.Request) {
	p, e := skillmanager.Get(s.skillLibrary(), r.PathValue("name"))
	if e != nil {
		skillError(w, e)
		return
	}
	if want := r.URL.Query().Get("digest"); want != "" && want != p.Digest {
		skillError(w, skillmanager.ErrConflict)
		return
	}
	b, e := skillmanager.Export(p)
	if e != nil {
		skillError(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+p.Name+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(b)
}
func (s *Server) removeSkill(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Revision *uint64 `json:"revision"`
		Digest   string  `json:"digest"`
	}
	if e := decodeJSON(w, r, &q); e != nil || q.Revision == nil {
		writeError(w, 400, "A current library revision and digest are required")
		return
	}
	v, e := skillmanager.Remove(s.skillLibrary(), r.PathValue("name"), q.Digest, *q.Revision)
	if e != nil {
		skillError(w, e)
		return
	}
	writeJSON(w, 200, v)
}

const skillWorkerScript = `exec "$HOME/.local/share/relay/bin/relay" skill-worker`

func (s *Server) hostSkills(w http.ResponseWriter, r *http.Request) {
	request := skillmanager.WorkerRequest{Action: "status", Name: r.PathValue("name")}
	if r.Method != "GET" && r.PathValue("name") != "" {
		var q struct {
			Digest string `json:"digest"`
		}
		if e := decodeJSON(w, r, &q); e != nil {
			writeError(w, 400, e.Error())
			return
		}
		request.Digest = q.Digest
		switch {
		case r.Method == "DELETE":
			request.Action = "remove"
		case strings.HasSuffix(r.URL.Path, "/reconcile"):
			request.Action = "reconcile"
		default:
			request.Action = "deploy"
		}
	}
	if r.Method == "GET" || strings.HasSuffix(r.URL.Path, "/check") {
		request.Name = r.URL.Query().Get("name")
	}
	if request.Name != "" && !skillmanager.ValidName(request.Name) {
		writeError(w, 400, "Invalid skill name")
		return
	}
	if request.Action == "deploy" {
		p, e := skillmanager.Get(s.skillLibrary(), request.Name)
		if e != nil {
			skillError(w, e)
			return
		}
		if p.Digest != request.Digest {
			skillError(w, skillmanager.ErrConflict)
			return
		}
		request.Package = &p
	}
	id := r.PathValue("id")
	local := id == "local"
	if local && !s.hasLocalMastermind() {
		writeError(w, 404, "Machine not found")
		return
	}
	s.mu.Lock()
	l := s.links[id]
	_, exists := s.store.get(id)
	if !local && (!exists || l == nil || l.conn == nil || !l.conn.Ready()) {
		s.mu.Unlock()
		writeError(w, 503, "Connect to this machine before managing skills")
		return
	}
	if l != nil && l.repairing {
		s.mu.Unlock()
		writeError(w, 409, "Wait for the current machine operation to finish")
		return
	}
	select {
	case s.repairSlots <- struct{}{}:
	default:
		s.mu.Unlock()
		writeError(w, 429, "Other machine operations are in progress")
		return
	}
	if l != nil {
		l.repairing = true
	}
	s.connectWG.Add(1)
	s.mu.Unlock()
	defer s.connectWG.Done()
	defer func() {
		<-s.repairSlots
		s.mu.Lock()
		if l != nil {
			l.repairing = false
		}
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	var result skillmanager.WorkerResult
	var err error
	if local {
		var home, base string
		home, err = os.UserHomeDir()
		if err == nil {
			base, err = skillmanager.DefaultHostStore()
		}
		if err == nil {
			result, err = skillmanager.Worker(base, home, request)
		}
	} else {
		if r.Method != "GET" {
			err = l.conn.RefreshCLI(ctx, s.config.BinaryDir, s.config.Version)
		}
		if err == nil {
			payload, _ := json.Marshal(request)
			var output []byte
			output, err = l.conn.Run(ctx, skillWorkerScript, bytes.NewReader(payload))
			if err == nil {
				if len(output) > 128<<10 {
					err = errors.New("Skill deployment response exceeded its limit")
				} else {
					err = json.Unmarshal(output, &result)
					if err == nil && result.Protocol != 1 {
						err = errors.New("Unsupported skill worker response")
					}
				}
			}
		}
	}
	s.mu.Lock()
	same := s.links[id] == l
	s.mu.Unlock()
	if !same {
		writeError(w, 503, "Machine connection changed. Deployment may have completed; check this machine before retrying.")
		return
	}
	if err != nil {
		if r.Method == "GET" {
			writeError(w, 503, "Skills could not be checked. Choose Check deployment to prepare the current skill manager; existing skills are preserved.")
		} else {
			writeError(w, 409, "Skill operation was not fully confirmed. Check deployment and review existing files before retrying. "+boundedSkillError(err))
		}
		return
	}
	writeJSON(w, 200, result)
}
func boundedSkillError(err error) string {
	message := err.Error()
	if len(message) > 1200 {
		return "See the machine's skill deployment status."
	}
	return message
}
