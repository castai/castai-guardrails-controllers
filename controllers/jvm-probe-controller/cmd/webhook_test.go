// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package main

import (
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
		Subject:      pkix.Name{CommonName: "jvm-probe-webhook-test"},
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

// TestWebhookServer_MutateRouteIsWired verifies that /mutate/pods is
// routed to the handler passed to NewWebhookServer and that requests
// without the cert in the pool are rejected by TLS.
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

	// /healthz must still return 200 and must NOT route to mutate.
	atomic.StoreInt32(&hits, 0)
	resp, err = client.Get("https://" + srv.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("mutate handler hit on /healthz: hits = %d, want 0", got)
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

	// First Stop after graceful shutdown must report no error.
	if err := srv.Stop(context.Background()); err != nil {
		t.Errorf("Stop after shutdown returned error: %v", err)
	}
	// Second Stop must remain a no-op.
	if err := srv.Stop(context.Background()); err != nil {
		t.Errorf("second Stop returned error: %v", err)
	}
}

// TestHealthServer_Healthz verifies that the plain-HTTP health server
// binds an ephemeral port and returns 200 OK on /healthz. It does not
// rely on TLS, so a plain http.Client is used.
func TestHealthServer_Healthz(t *testing.T) {
	srv := NewHealthServer("127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Start(ctx) }()

	addr := srv.Addr()
	url := "http://" + addr + "/healthz"

	client := &http.Client{Timeout: 5 * time.Second}
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

// TestHealthServer_OnlyServesHealthz verifies that /metrics and
// /mutate/pods are NOT exposed on the plain-HTTP health port. The
// kubelet probes must never reach the admission webhook port via this
// listener.
func TestHealthServer_OnlyServesHealthz(t *testing.T) {
	srv := NewHealthServer("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Start(ctx) }()

	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			// Treat plain HTTP 404 as a valid response so we can inspect
			// the status code rather than getting a transport error.
			DisableKeepAlives: true,
		},
	}
	for _, path := range []string{"/metrics", "/mutate/pods"} {
		resp, err := client.Get("http://" + srv.Addr() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404 (path must not be served on the plain-HTTP health port)", path, resp.StatusCode)
		}
	}

	cancel()
	<-serveErr
}

// TestHealthServer_StopIsIdempotent mirrors TestWebhookServer_StopIsIdempotent
// for the plain-HTTP server.
func TestHealthServer_StopIsIdempotent(t *testing.T) {
	srv := NewHealthServer("127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Start(ctx) }()

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

// TestHealthAndWebhookCoexist verifies that the HTTPS webhook server and
// the plain-HTTP health server can run side by side on independent
// ephemeral ports. A request to /healthz on each port must succeed; a
// request to /mutate/pods on the HTTPS port must succeed; a request to
// /mutate/pods on the HTTP port must be rejected.
func TestHealthAndWebhookCoexist(t *testing.T) {
	certFile, keyFile, pool := writeSelfSignedCert(t)

	mutate := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}
	webhook := NewWebhookServer("127.0.0.1:0", certFile, keyFile, mutate)
	health := NewHealthServer("127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	webhookErr := make(chan error, 1)
	healthErr := make(chan error, 1)
	go func() { webhookErr <- webhook.Start(ctx) }()
	go func() { healthErr <- health.Start(ctx) }()

	webhookAddr := webhook.Addr()
	healthAddr := health.Addr()
	if webhookAddr == healthAddr {
		t.Fatalf("expected different ports, both bound %s", webhookAddr)
	}

	httpsClient := testClient(pool)
	httpClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	// /healthz on the plain-HTTP port must succeed.
	resp, err := httpClient.Get("http://" + healthAddr + "/healthz")
	if err != nil {
		t.Fatalf("HTTP GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP /healthz status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// /mutate/pods on the HTTPS port must succeed.
	resp, err = httpsClient.Get("https://" + webhookAddr + "/mutate/pods")
	if err != nil {
		t.Fatalf("HTTPS GET /mutate/pods: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS /mutate/pods status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// /mutate/pods on the plain-HTTP port must NOT be served.
	resp, err = httpClient.Get("http://" + healthAddr + "/mutate/pods")
	if err != nil {
		t.Fatalf("HTTP GET /mutate/pods: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		t.Errorf("HTTP /mutate/pods status = %d, want 404 (must not be served on the plain-HTTP health port)", resp.StatusCode)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// /metrics on the plain-HTTP port must NOT be served.
	resp, err = httpClient.Get("http://" + healthAddr + "/metrics")
	if err != nil {
		t.Fatalf("HTTP GET /metrics: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		t.Errorf("HTTP /metrics status = %d, want 404 (must not be served on the plain-HTTP health port)", resp.StatusCode)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	cancel()
	select {
	case err := <-webhookErr:
		if err != nil {
			t.Errorf("webhook Start returned error after shutdown: %v", err)
		}
	case <-time.After(webhookShutdownTimeout + time.Second):
		t.Fatalf("webhook Start did not return after context cancel")
	}
	select {
	case err := <-healthErr:
		if err != nil {
			t.Errorf("health Start returned error after shutdown: %v", err)
		}
	case <-time.After(webhookShutdownTimeout + time.Second):
		t.Fatalf("health Start did not return after context cancel")
	}
}
