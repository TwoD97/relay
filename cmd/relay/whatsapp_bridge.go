package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/TwoD97/relay/internal/whatsappbridge"
)

func whatsappBridge(args []string) error {
	f := flag.NewFlagSet("whatsapp-bridge", flag.ContinueOnError)
	config := f.String("config", "", "Private JSON configuration file; secrets are never passed as flags")
	state := f.String("state-dir", defaultDir("relay-whatsapp-bridge"), "Private durable webhook queue")
	address := f.String("listen", "127.0.0.1:8787", "Bind address; HTTP is allowed only on loopback behind an HTTPS reverse proxy")
	cert := f.String("tls-cert", "", "TLS certificate for direct HTTPS hosting")
	key := f.String("tls-key", "", "TLS private key for direct HTTPS hosting")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *config == "" {
		return errors.New("Use relay whatsapp-bridge --config /private/bridge.json; see docs/WHATSAPP_BUSINESS.md")
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil {
		return errors.New("Invalid bridge listen address")
	}
	if (*cert == "") != (*key == "") {
		return errors.New("Supply both TLS certificate and key")
	}
	if *cert == "" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("Unencrypted bridge HTTP must bind to a literal loopback address behind an HTTPS reverse proxy")
		}
	}
	cfg, err := whatsappbridge.LoadConfig(*config)
	if err != nil {
		return err
	}
	bridge, err := whatsappbridge.New(*state, cfg)
	if err != nil {
		return err
	}
	defer bridge.Close()
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return errors.New("Could not bind the bridge address")
	}
	defer listener.Close()
	server := &http.Server{Handler: bridge.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if server.Shutdown(shutdown) != nil {
				_ = server.Close()
			}
		case <-done:
		}
	}()
	defer func() { close(done); <-shutdownDone }()
	fmt.Fprintln(os.Stderr, "WhatsApp Business bridge listening on", listener.Addr(), "— desktop control remains private")
	if *cert != "" {
		err = server.ServeTLS(listener, *cert, *key)
	} else {
		err = server.Serve(listener)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.New("Bridge listener stopped; check its TLS and host configuration")
}
