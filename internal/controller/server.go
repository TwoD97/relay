package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/TwoD97/relay/internal/transport"
)

type Config struct {
	StateDir, BinaryDir, Version, Address, LocalSocket string
	Assets                                             fs.FS
	startConnection                                    func(context.Context, transport.Config) (*transport.Connection, error)
}
type link struct {
	conn          *transport.Connection
	proxy         *httputil.ReverseProxy
	httpTransport *http.Transport
	cancel        context.CancelFunc
	repairing     bool
}

type Server struct {
	config                          Config
	store                           *hostStore
	ctx                             context.Context
	cancel                          context.CancelFunc
	mu                              sync.Mutex
	links                           map[string]*link
	port                            string
	launchToken, sessionToken, csrf string
	authMu                          sync.Mutex
	auditMu                         sync.Mutex
	launchTokens                    map[[32]byte]time.Time
	handler                         http.Handler
	directorySlots                  chan struct{}
	connectSlots                    chan struct{}
	connectWG                       sync.WaitGroup
	startConnection                 func(context.Context, transport.Config) (*transport.Connection, error)
	repairSlots                     chan struct{}
	maintenance                     *maintenanceStore
	maintenanceWG                   sync.WaitGroup
}

func New(cfg Config) (*Server, error) {
	host, port, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("client must bind to a loopback IP address")
	}
	store, err := openStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	maintenance, err := openMaintenanceStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{config: cfg, store: store, ctx: ctx, cancel: cancel, links: map[string]*link{}, port: port, launchToken: randomID(), sessionToken: randomID(), csrf: randomID(), directorySlots: make(chan struct{}, 4)}
	s.maintenance = maintenance
	s.connectSlots = make(chan struct{}, 4)
	s.repairSlots = make(chan struct{}, 2)
	s.startConnection = cfg.startConnection
	if s.startConnection == nil {
		s.startConnection = transport.Start
	}
	s.launchTokens = make(map[[32]byte]time.Time)
	s.addLoginTokenLocked(s.launchToken, time.Now())
	if cfg.LocalSocket != "" {
		s.links["local"] = s.makeProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", cfg.LocalSocket)
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"csrf": s.csrf, "version": cfg.Version})
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/reconnect", s.reconnectHosts)
	mux.HandleFunc("POST /api/hosts", s.addHost)
	mux.HandleFunc("POST /api/hosts/{id}/connect", s.connectHost)
	mux.HandleFunc("POST /api/hosts/{id}/disconnect", s.disconnectHost)
	mux.HandleFunc("POST /api/hosts/{id}/repair-runtime", s.repairRuntime)
	mux.HandleFunc("POST /api/hosts/{id}/project-context", s.prepareProjectContext)
	mux.HandleFunc("POST /api/hosts/{id}/harnesses/{harness}/{action}", s.maintainHostHarness)
	mux.HandleFunc("DELETE /api/hosts/{id}", s.forgetHost)
	mux.HandleFunc("GET /api/hosts/{id}/setup-terminal", s.setupTerminal)
	mux.HandleFunc("GET /api/hosts/{id}/directories", s.directories)
	mux.HandleFunc("/api/hosts/{id}/runtime/{route...}", s.runtimeProxy)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeError(w, 404, "Unknown API endpoint") })
	if cfg.Assets != nil {
		files := http.FileServer(http.FS(cfg.Assets))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "Method not allowed", 405)
				return
			}
			if r.URL.Path != "/" {
				if _, err := fs.Stat(cfg.Assets, strings.TrimPrefix(path.Clean(r.URL.Path), "/")); err != nil {
					http.NotFound(w, r)
					return
				}
			}
			files.ServeHTTP(w, r)
		})
	}
	s.handler = s.secure(s.audit(mux))
	s.ReconnectSaved()
	s.maintenanceWG.Add(1)
	go s.monitorMaintenance()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }
func (s *Server) LoginURL() string {
	return "http://" + s.config.Address + "/auth?token=" + s.launchToken
}
func (s *Server) Close() error {
	s.cancel()
	s.mu.Lock()
	links := s.links
	s.links = map[string]*link{}
	s.mu.Unlock()
	for _, l := range links {
		if l.cancel != nil {
			l.cancel()
		}
		if l.conn != nil {
			l.conn.Close()
		}
		if l.httpTransport != nil {
			l.httpTransport.CloseIdleConnections()
		}
	}
	s.connectWG.Wait()
	s.maintenanceWG.Wait()
	return nil
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	hosts := s.store.list()
	if s.config.LocalSocket != "" {
		hosts = append([]Host{{ID: "local", Name: "This computer", Target: "local", Status: "online", Stage: "Local runtime"}}, hosts...)
	}
	writeJSON(w, 200, map[string]any{"version": s.config.Version, "hosts": hosts, "maintenanceJobs": s.maintenance.list()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}

func (s *Server) makeProxy(dial func(context.Context, string, string) (net.Conn, error)) *link {
	t := &http.Transport{DialContext: dial, MaxIdleConns: 8, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 20, IdleConnTimeout: 60 * time.Second, ResponseHeaderTimeout: 30 * time.Second, DisableCompression: true}
	p := &httputil.ReverseProxy{Transport: t, FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.Out.URL = &url.URL{Scheme: "http", Host: "relay-runtime", Path: "/api/" + pr.In.PathValue("route"), RawQuery: pr.In.URL.RawQuery}
		pr.Out.Host = pr.In.Host
		pr.Out.Header.Del("Cookie")
		pr.Out.Header.Del("Authorization")
		pr.Out.Header.Del("X-Relay-CSRF")
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, 502, "Host connection interrupted. Reconnect before retrying; terminal input is never queued.")
	}}
	return &link{proxy: p, httpTransport: t}
}

func (s *Server) runtimeProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	l := s.links[id]
	var proxy *httputil.ReverseProxy
	if l != nil {
		proxy = l.proxy
	}
	s.mu.Unlock()
	if proxy == nil {
		writeError(w, 503, "Host is disconnected")
		return
	}
	if id != "local" {
		h, ok := s.store.get(id)
		if !ok || h.Status != "online" {
			writeError(w, 503, "Host is not ready")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	proxy.ServeHTTP(w, r)
}
