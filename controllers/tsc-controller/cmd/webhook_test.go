// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// writeSelfSignedCert generates a self-signed certificate for 127.0.0.1 and
// writes the PEM-encoded cert and key to a temporary directory. The caller
// is responsible for removing the directory when the test finishes.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string, certPool *x509.CertPool) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tsc-webhook-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatalf("failed to add generated cert to pool")
	}
	return certFile, keyFile, pool
}

// testClient returns an *http.Client configured to trust the supplied cert
// pool. Used so HTTPS calls succeed against the self-signed test cert
// without disabling verification globally.
func testClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool}, //nolint:gosec
		},
	}
}

// TestWebhookServer_Healthz verifies that the server starts on an
// ephemeral port, serves TLS, and /healthz returns 200 OK.
func TestWebhookServer_Healthz(t *testing.T) {
	certFile, keyFile, pool := writeSelfSignedCert(t)

	stub := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}
	srv := NewWebhookServer("127.0.0.1:0", certFile, keyFile, stub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Start(ctx) }()

	// Wait for the listener to be open so we know the bound address.
	addr := srv.Addr()
	url := "https://" + addr + "/healthz"

	client := testClient(pool)
	// Poll briefly because Start returns control as soon as the listener
	// is open; the goroutine may still be entering ServeTLS.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /healthz never succeeded: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Graceful shutdown via context cancel must not return an error.
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Start returned error after shutdown: %v", err)
		}
	case <-time.After(webhookShutdownTimeout + time.Second):
		t.Fatalf("Start did not return after context cancel")
	}
}

// TestWebhookServer_Metrics verifies that /metrics renders the counters
// in the expected plain-text format and bumps on each request.
func TestWebhookServer_Metrics(t *testing.T) {
	certFile, keyFile, pool := writeSelfSignedCert(t)
	stub := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}
	srv := NewWebhookServer("127.0.0.1:0", certFile, keyFile, stub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Start(ctx) }()

	client := testClient(pool)
	// Trigger the instrument wrapper so the requests counter bumps. The
	// body is a valid AdmissionReview with an ownerless Pod; the
	// wrapper classifies it as a normal request (not error/bypass).
	body := []byte(`{"request":{"uid":"x","object":{"raw":"eyJhcGlWZXJzaW9uIjoidjEiLCJraW5kIjoiUG9kIiwibWV0YWRhdGEiOnt9LCJzcGVjIjp7ImNvbnRhaW5lcnMiOlt7Im5hbWUiOiJjIiwiaW1hZ2UiOiJpIn1dfX0="}}}`)
	if r, err := client.Post("https://" + srv.Addr()+"/mutate/pods", "application/json", bytes.NewReader(body)); err != nil {
		t.Fatalf("POST /mutate/pods: %v", err)
	} else {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}

	resp, err := client.Get("https://" + srv.Addr() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	// After one mutate request, the requests counter is at least 1.
	s := string(bodyBytes)
	wantLines := []string{
		"tsc_admission_requests_total 1",
		"tsc_mutations_total 0",
		"tsc_bypasses_total 0",
		"tsc_errors_total 0",
	}
	for _, want := range wantLines {
		if !contains(s, want) {
			t.Errorf("/metrics body missing %q\nfull body:\n%s", want, s)
		}
	}

	cancel()
	<-serveErr
}

// TestWebhookServer_StopIsIdempotent verifies that calling Stop after the
// server has already been shut down by context cancel returns nil.
func TestWebhookServer_StopIsIdempotent(t *testing.T) {
	certFile, keyFile, _ := writeSelfSignedCert(t)
	srv := NewWebhookServer("127.0.0.1:0", certFile, keyFile, func(http.ResponseWriter, *http.Request) {})

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Start(ctx) }()

	// Wait for the listener so the server is fully started.
	_ = srv.Addr()

	cancel()
	<-serveErr

	if err := srv.Stop(context.Background()); err != nil {
		t.Errorf("Stop after shutdown returned error: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Errorf("second Stop returned error: %v", err)
	}
}

// TestWebhookServer_MutateRouteIsWired verifies that /mutate/pods is
// routed to the handler passed to NewWebhookServer and that /healthz
// does not route to the mutate handler.
func TestWebhookServer_MutateRouteIsWired(t *testing.T) {
	certFile, keyFile, pool := writeSelfSignedCert(t)
	var hits int32
	mutate := func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}
	srv := NewWebhookServer("127.0.0.1:0", certFile, keyFile, mutate)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Start(ctx) }()

	client := testClient(pool)
	resp, err := client.Get("https://" + srv.Addr() + "/mutate/pods")
	if err != nil {
		t.Fatalf("GET /mutate/pods: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/mutate/pods status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("mutate handler hits = %d, want 1", got)
	}
	cancel()
	<-serveErr
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
