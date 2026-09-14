// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// webhookShutdownTimeout bounds how long Start waits for in-flight requests
// to complete when the parent context is canceled.
const webhookShutdownTimeout = 30 * time.Second

// WebhookServer is the HTTPS admission webhook server. It is intentionally
// minimal: it owns a single mux, loads TLS credentials from disk on Start,
// and exits cleanly when its context is canceled or Stop is called.
//
// The server is designed to run on every replica — there is no built-in
// leader election. The admission webhook must accept traffic on all pods so
// that Pod creation is never lost during a leader transition.
type WebhookServer struct {
	addr     string
	certFile string
	keyFile  string
	mux      *http.ServeMux
	server   *http.Server

	listener net.Listener
	ready    chan struct{}
}

// NewWebhookServer builds a WebhookServer bound to addr. addr may include a
// port of ":0" (or "127.0.0.1:0") for tests that need an ephemeral port.
// mutateHandler is wired to /mutate/pods; /healthz is always served and
// returns 200 OK.
func NewWebhookServer(addr, certFile, keyFile string, mutateHandler http.HandlerFunc) *WebhookServer {
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate/pods", mutateHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return &WebhookServer{
		addr:     addr,
		certFile: certFile,
		keyFile:  keyFile,
		mux:      mux,
		ready:    make(chan struct{}),
	}
}

// Start binds the listener and serves until ctx is canceled or the server
// returns an error. It blocks for the lifetime of the server; callers run
// it in a goroutine and signal shutdown via ctx or Stop.
//
// When ctx is canceled, Start performs a graceful shutdown bounded by
// webhookShutdownTimeout and returns nil.
func (s *WebhookServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("webhook listen on %s: %w", s.addr, err)
	}
	s.listener = ln

	s.server = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		err := s.server.ServeTLS(ln, s.certFile, s.keyFile)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// Publishing readiness is the happens-before edge for any caller that
	// reads Addr() or Listener(): after close(s.ready) returns, the
	// listener field is visible.
	close(s.ready)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), webhookShutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			<-serveErr
			return fmt.Errorf("webhook shutdown: %w", err)
		}
		// Drain ServeTLS so the goroutine always exits before Start returns.
		<-serveErr
		return nil
	case err := <-serveErr:
		return err
	}
}

// Stop performs a graceful shutdown of the server, bounded by the supplied
// context's deadline. It is safe to call when the server has already been
// stopped or was never started; subsequent calls are no-ops.
func (s *WebhookServer) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	err := s.server.Shutdown(ctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("webhook shutdown: %w", err)
	}
	return nil
}

// Addr returns the actual TCP address the server is listening on. It blocks
// until the listener has been opened by Start. If Start has not been
// called, Addr blocks until the channel is closed (which it never is in
// that case) — callers must invoke Start first.
func (s *WebhookServer) Addr() string {
	<-s.ready
	return s.listener.Addr().String()
}
