package mock

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	base    = "/api/v1/accounts/acc_local"
	domains = base + "/upstreams/upstream_local/custom_domains"
	token   = "scd_test_token"
)

type harness struct {
	t     *testing.T
	srv   *Server
	api   *httptest.Server
	proxy *httptest.Server
}

// newHarness starts the API and proxy with default config, a tiny DNS delay
// and optional overrides.
func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	cfg, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.DNSDelay = 10 * time.Millisecond
	cfg.WebhookBackoff = 10 * time.Millisecond
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, srv: srv, api: httptest.NewServer(srv.APIHandler()), proxy: httptest.NewServer(srv.ProxyHandler())}
	t.Cleanup(h.api.Close)
	t.Cleanup(h.proxy.Close)
	return h
}

// raw sends a request to the API. body may be nil, url.Values (form) or
// anything else (JSON).
func (h *harness) raw(method, path string, body any, auth string) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		rd, ct = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	case string:
		rd, ct = strings.NewReader(b), "application/json"
	default:
		buf, _ := json.Marshal(b)
		rd, ct = bytes.NewReader(buf), "application/json"
	}
	req, _ := http.NewRequest(method, h.api.URL+path, rd)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// call sends an authenticated request and decodes a JSON object response.
func (h *harness) call(method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	status, out := h.raw(method, path, body, "Bearer "+token)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		h.t.Fatalf("%s %s: response is not a JSON object: %s", method, path, out)
	}
	return status, m
}

// mustCall is call that fails the test on an unexpected status.
func (h *harness) mustCall(want int, method, path string, body any) map[string]any {
	h.t.Helper()
	status, m := h.call(method, path, body)
	if status != want {
		h.t.Fatalf("%s %s: status %d, want %d: %v", method, path, status, want, m)
	}
	return m
}

func (h *harness) createDomain(host string, extra map[string]any) map[string]any {
	h.t.Helper()
	body := map[string]any{"host": host}
	for k, v := range extra {
		body[k] = v
	}
	return h.mustCall(201, "POST", domains, body)
}

func (h *harness) getDomain(uuid string) map[string]any {
	h.t.Helper()
	return h.mustCall(200, "GET", domains+"/"+uuid, nil)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
