package localagent

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (s *Server) resolveCwd(path string) (string, error) {
	if len(path) > 4096 || !utf8.ValidString(path) || strings.ContainsFunc(path, unicode.IsControl) {
		return "", errors.New("invalid directory path")
	}
	if path == "" || path == "~" || path == "$HOME" {
		path = s.home
	} else if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		path = filepath.Join(s.home, path[2:])
	} else if strings.HasPrefix(path, "$HOME/") || strings.HasPrefix(path, "$HOME\\") {
		path = filepath.Join(s.home, path[6:])
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(s.home, path)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(path), nil
}
func (s *Server) directories(w http.ResponseWriter, r *http.Request) {
	path, err := s.resolveCwd(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if len(prefix) > 255 || strings.ContainsAny(prefix, "/\\\x00\r\n") {
		fail(w, 400, errors.New("invalid directory prefix"))
		return
	}
	f, err := os.Open(path)
	if err != nil {
		fail(w, 403, err)
		return
	}
	defer f.Close()
	type entry struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Symlink bool   `json:"symlink,omitempty"`
	}
	all := []entry{}
	truncated := false
	hidden := r.URL.Query().Get("hidden") == "true" || strings.HasPrefix(prefix, ".")
	for scanned := 0; scanned < 20000; {
		if r.Context().Err() != nil {
			return
		}
		items, e := f.ReadDir(128)
		scanned += len(items)
		for _, item := range items {
			if !strings.HasPrefix(item.Name(), prefix) || (!hidden && strings.HasPrefix(item.Name(), ".")) || strings.ContainsFunc(item.Name(), unicode.IsControl) {
				continue
			}
			full := filepath.Join(path, item.Name())
			directory := item.IsDir()
			link := item.Type()&os.ModeSymlink != 0
			if link {
				info, e := os.Stat(full)
				directory = e == nil && info.IsDir()
			}
			if directory {
				if len(all) >= 200 {
					truncated = true
					continue
				}
				all = append(all, entry{item.Name(), full, link})
			}
		}
		if e != nil {
			break
		}
		if scanned >= 20000 {
			truncated = true
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	var parent *string
	if p := filepath.Dir(path); p != path {
		parent = &p
	}
	jsonResponse(w, 200, struct {
		Path        string  `json:"path"`
		Home        string  `json:"home"`
		Parent      *string `json:"parent"`
		Directories []entry `json:"directories"`
		Truncated   bool    `json:"truncated"`
	}{path, s.home, parent, all, truncated})
}
