package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TwoD97/relay/internal/transport"
)

func (s *Server) directories(w http.ResponseWriter, r *http.Request) {
	values, err := parseDirectoryQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	l := s.links[id]
	var connection *transport.Connection
	if l != nil {
		connection = l.conn
	}
	s.mu.Unlock()
	if l == nil || (id != "local" && (connection == nil || !connection.Ready())) {
		writeError(w, http.StatusServiceUnavailable, "Host is disconnected. Reconnect to browse folders.")
		return
	}
	if id != "local" {
		host, ok := s.store.get(id)
		if !ok || host.Status != "online" {
			writeError(w, http.StatusServiceUnavailable, "Host is not ready to browse folders.")
			return
		}
	}
	select {
	case s.directorySlots <- struct{}{}:
		defer func() { <-s.directorySlots }()
	default:
		writeError(w, http.StatusTooManyRequests, "Folder browsing is busy. Try again shortly.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	var listing transport.DirectoryListing
	if id == "local" {
		listing, err = localDirectories(ctx, values)
	} else {
		listing, err = connection.ListDirectories(ctx, values)
	}
	s.mu.Lock()
	current := s.links[id] == l
	s.mu.Unlock()
	if !current {
		writeError(w, http.StatusServiceUnavailable, "Host connection changed. Browse again after reconnecting.")
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, transport.ErrDirectoryNotFound), errors.Is(err, os.ErrNotExist):
			writeError(w, http.StatusNotFound, "Folder does not exist.")
		case errors.Is(err, transport.ErrDirectoryPermission), errors.Is(err, os.ErrPermission):
			writeError(w, http.StatusForbidden, "Permission denied opening this folder.")
		case errors.Is(err, transport.ErrNotDirectory):
			writeError(w, http.StatusBadRequest, "Path is not a folder.")
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			writeError(w, http.StatusGatewayTimeout, "Folder lookup timed out or was cancelled.")
		default:
			writeError(w, http.StatusBadGateway, "Could not read folders from this host. Check the path and connection.")
		}
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

func parseDirectoryQuery(r *http.Request) (transport.DirectoryQuery, error) {
	if len(r.URL.RawQuery) > 16384 {
		return transport.DirectoryQuery{}, errors.New("folder browsing query is too long")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return transport.DirectoryQuery{}, errors.New("invalid folder browsing query")
	}
	for key, items := range values {
		if (key != "path" && key != "prefix" && key != "hidden") || len(items) != 1 {
			return transport.DirectoryQuery{}, errors.New("invalid folder browsing query")
		}
	}
	hidden := values.Get("hidden")
	if hidden != "" && hidden != "true" && hidden != "false" {
		return transport.DirectoryQuery{}, errors.New("hidden must be true or false")
	}
	query := transport.DirectoryQuery{Path: values.Get("path"), Prefix: values.Get("prefix"), Hidden: hidden == "true"}
	return query, query.Validate()
}

func localDirectories(ctx context.Context, query transport.DirectoryQuery) (transport.DirectoryListing, error) {
	if err := query.Validate(); err != nil {
		return transport.DirectoryListing{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return transport.DirectoryListing{}, err
	}
	directory := query.Path
	if directory == "" || directory == "~" {
		directory = home
	} else if strings.HasPrefix(directory, "~/") {
		directory = filepath.Join(home, directory[2:])
	}
	if err := ctx.Err(); err != nil {
		return transport.DirectoryListing{}, err
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return transport.DirectoryListing{}, err
	}
	file, err := os.Open(directory)
	if err != nil {
		return transport.DirectoryListing{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return transport.DirectoryListing{}, err
	}
	if !info.IsDir() {
		return transport.DirectoryListing{}, transport.ErrNotDirectory
	}
	listing := transport.DirectoryListing{Home: home, Path: directory, Directories: []transport.DirectoryEntry{}}
	if parent := filepath.Dir(directory); parent != directory {
		listing.Parent = &parent
	}
	// Bound enumeration work as well as the number of returned rows. Prefix
	// filtering happens before the result limit so ordinary completion searches
	// can reach matches beyond an unfiltered first page.
	for scanned := 0; scanned < 10000; {
		if err := ctx.Err(); err != nil {
			return transport.DirectoryListing{}, err
		}
		entries, readErr := file.ReadDir(128)
		for _, entry := range entries {
			scanned++
			if !query.Matches(entry.Name()) {
				continue
			}
			entryPath := filepath.Join(directory, entry.Name())
			if len(entryPath) > 4096 {
				continue
			}
			isDirectory := entry.IsDir()
			if entry.Type()&os.ModeSymlink != 0 {
				info, err := os.Stat(entryPath)
				isDirectory = err == nil && info.IsDir()
			}
			if isDirectory {
				listing.Directories = append(listing.Directories, transport.DirectoryEntry{Name: entry.Name(), Path: entryPath})
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return transport.DirectoryListing{}, readErr
		}
		if scanned >= 10000 || len(listing.Directories) > transport.DirectoryLimit {
			listing.Truncated = true
			break
		}
	}
	transport.SortDirectories(&listing)
	return listing, nil
}
