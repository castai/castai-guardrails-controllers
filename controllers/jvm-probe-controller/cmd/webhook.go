// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
)

// admissionMetrics holds the in-memory counters exposed by /metrics. The
// counters are monotonic uint64s updated atomically so the /metrics
// handler never blocks the request path. No external Prometheus client
// is required — the endpoint returns plain text in a custom format.
type admissionMetrics struct {
	requests  atomic.Uint64
	mutations atomic.Uint64
	bypasses  atomic.Uint64
	errors    atomic.Uint64
}

// Snapshot returns the current counter values in a consistent order. The
// values are read individually; under high concurrency a counter read
// here may not perfectly match the count read a microsecond later from
// another counter. This is acceptable for an observability endpoint.
func (m *admissionMetrics) snapshot() (requests, mutations, bypasses, errs uint64) {
	return m.requests.Load(), m.mutations.Load(), m.bypasses.Load(), m.errors.Load()
}

// WriteText renders the counters in Prometheus-like plain text format.
// The format is intentionally trivial so operators can scrape it with
// curl or any metrics scraper without depending on a Prometheus client.
func (m *admissionMetrics) WriteText(w http.ResponseWriter) {
	requests, mutations, bypasses, errs := m.snapshot()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "jvm_probe_admission_requests_total %d\n", requests)
	fmt.Fprintf(w, "jvm_probe_mutations_total %d\n", mutations)
	fmt.Fprintf(w, "jvm_probe_bypasses_total %d\n", bypasses)
	fmt.Fprintf(w, "jvm_probe_errors_total %d\n", errs)
}

// instrument wraps mutateHandler so each call increments the appropriate
// counter. Classification is best-effort: a malformed body bumps errors;
// a body with the bypass annotation bumps bypasses; otherwise the
// response is inspected and a non-empty Patch bumps mutations.
//
// The wrapper buffers the response from the inner handler so it can both
// forward the bytes to the client and read them back for classification.
func (m *admissionMetrics) instrument(mutateHandler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.requests.Add(1)

		// Buffer the body so we can classify and then re-deliver it to
		// the inner handler untouched.
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		isError := false
		isBypass := false
		review := &admissionv1.AdmissionReview{}
		if err := json.Unmarshal(bodyBytes, review); err != nil ||
			review.Request == nil ||
			len(review.Request.Object.Raw) == 0 {
			isError = true
		} else {
			pod := &corev1.Pod{}
			if json.Unmarshal(review.Request.Object.Raw, pod) == nil &&
				IsBypassAnnotation(pod.Annotations) {
				isBypass = true
			}
		}

		rec := httptest.NewRecorder()
		mutateHandler(rec, r)
		for k, vv := range rec.Header() {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())

		switch {
		case isError:
			m.errors.Add(1)
		case isBypass:
			m.bypasses.Add(1)
		default:
			resp := &admissionv1.AdmissionReview{}
			if json.Unmarshal(rec.Body.Bytes(), resp) == nil &&
				resp.Response != nil &&
				len(resp.Response.Patch) > 0 {
				m.mutations.Add(1)
			}
		}
	}
}

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
	metrics  *admissionMetrics

	listener net.Listener
	ready    chan struct{}
}

// NewWebhookServer builds a WebhookServer bound to addr. addr may include a
// port of ":0" (or "127.0.0.1:0") for tests that need an ephemeral port.
// mutateHandler is wired to /mutate/pods; /healthz and /metrics are
// always served and return 200 OK.
func NewWebhookServer(addr, certFile, keyFile string, mutateHandler http.HandlerFunc) *WebhookServer {
	metrics := &admissionMetrics{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate/pods", metrics.instrument(mutateHandler))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { metrics.WriteText(w) })
	return &WebhookServer{
		addr:     addr,
		certFile: certFile,
		keyFile:  keyFile,
		mux:      mux,
		metrics:  metrics,
		ready:    make(chan struct{}),
	}
}

// Metrics exposes the live counters for tests and operators that need
// direct access (e.g., readiness probes). The returned struct shares
// state with the /metrics endpoint.
func (s *WebhookServer) Metrics() *admissionMetrics {
	return s.metrics
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

// HealthServer is a plain-HTTP listener that serves /healthz for kubelet
// probes. It is intentionally minimal: no TLS, no mutation handler, no
// metrics endpoint. Separating it from WebhookServer keeps the in-cluster
// probe traffic off the HTTPS admission port — kubelet's tcpSocket probes
// against the HTTPS port caused TLS handshake EOF log spam, while httpGet
// probes against a plain HTTP port give kubelet a clean liveness signal.
type HealthServer struct {
	addr string
	mux  *http.ServeMux
	server *http.Server

	listener net.Listener
	ready    chan struct{}
}

// NewHealthServer builds a HealthServer bound to addr. addr may include a
// port of ":0" (or "127.0.0.1:0") for tests that need an ephemeral port.
// Only /healthz is registered; /metrics and /mutate/pods are not exposed
// on this listener.
func NewHealthServer(addr string) *HealthServer {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return &HealthServer{
		addr:  addr,
		mux:   mux,
		ready: make(chan struct{}),
	}
}

// Start binds the listener and serves until ctx is canceled or the server
// returns an error. It mirrors WebhookServer.Start's contract so callers
// can drive both servers with the same pattern.
func (s *HealthServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("health listen on %s: %w", s.addr, err)
	}
	s.listener = ln

	s.server = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		err := s.server.Serve(ln)
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
			return fmt.Errorf("health shutdown: %w", err)
		}
		<-serveErr
		return nil
	case err := <-serveErr:
		return err
	}
}

// Stop performs a graceful shutdown of the server, bounded by the supplied
// context's deadline. It is safe to call when the server has already been
// stopped or was never started; subsequent calls are no-ops.
func (s *HealthServer) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	err := s.server.Shutdown(ctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("health shutdown: %w", err)
	}
	return nil
}

// Addr returns the actual TCP address the health server is listening on.
// It blocks until the listener has been opened by Start.
func (s *HealthServer) Addr() string {
	<-s.ready
	return s.listener.Addr().String()
}
