package mock

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type delivery struct {
	query  url.Values
	header http.Header
	body   map[string]any
}

// webhookReceiver fails the first `failures` requests with 500.
func webhookReceiver(t *testing.T, failures int32) (*httptest.Server, chan delivery) {
	ch := make(chan delivery, 50)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= failures {
			w.WriteHeader(500)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("webhook body is not JSON: %s", raw)
		}
		ch <- delivery{query: r.URL.Query(), header: r.Header.Clone(), body: body}
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func next(t *testing.T, ch chan delivery) delivery {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(3 * time.Second):
		t.Fatal("no webhook delivered")
		return delivery{}
	}
}

func TestWebhooks(t *testing.T) {
	recv, ch := webhookReceiver(t, 0)
	h := newHarness(t, func(c *Config) { c.WebhookURL = recv.URL + "/hooks?source=scd&token=old" })

	up := h.mustCall(201, "POST", base+"/upstreams", map[string]any{"host": "app.mysaas.com", "port": 8443})
	d := next(t, ch)
	if d.query.Get("token") != "local_webhook_token" || d.query.Get("source") != "scd" || len(d.query["token"]) != 1 {
		t.Errorf("query = %v", d.query)
	}
	if d.header.Get("Content-Type") != "application/json" || d.header.Get("X-Scd-Webhook-Token") != "local_webhook_token" {
		t.Errorf("headers = %v", d.header)
	}
	if d.body["event"] != "upstream_created" {
		t.Fatalf("event = %v", d.body["event"])
	}
	upBody := d.body["upstream"].(map[string]any)
	wantUp := []string{"created_at", "uuid", "host", "port", "tls", "bubble_io"}
	if got := keys(upBody); !sameKeys(got, wantUp) {
		t.Errorf("upstream keys = %v", got)
	}
	if upBody["uuid"] != up["uuid"] || upBody["port"] != float64(8443) || upBody["host"] != "app.mysaas.com" {
		t.Errorf("upstream payload = %v", upBody)
	}
	if _, err := time.Parse("2006-01-02T15:04:05Z", upBody["created_at"].(string)); err != nil {
		t.Errorf("created_at %v is not UTC ISO 8601 with seconds", upBody["created_at"])
	}

	dom := h.createDomain("acme.localhost", map[string]any{"instructions_recipient": "steve@apple.com"})
	var events []string
	var last map[string]any
	for range 4 {
		d := next(t, ch)
		events = append(events, d.body["event"].(string))
		last = d.body["domain"].(map[string]any)
	}
	wantEvents := []string{"domain_created", "domain_dns_proxy_record_valid", "tls_certificate_issued", "domain_proxy_deployment_complete"}
	if !slices.Equal(events, wantEvents) {
		t.Errorf("events = %v, want %v", events, wantEvents)
	}
	wantDomain := []string{"created_at", "uuid", "upstream_uuid", "host", "challenge_type",
		"instructions_recipient", "last_dns_check_at", "proxy_deployment_complete", "proxy_deployment_completed_at"}
	if got := keys(last); !sameKeys(got, wantDomain) {
		t.Errorf("domain keys = %v", got)
	}
	if last["uuid"] != dom["uuid"] || last["proxy_deployment_complete"] != true ||
		last["instructions_recipient"] != "steve@apple.com" || last["last_dns_check_at"] == nil {
		t.Errorf("domain payload = %v", last)
	}

	// Status changes only: a repeated verify sends nothing, a flip sends one event.
	h.mustCall(200, "POST", domains+"/"+dom["uuid"].(string)+"/verify_dns_records", nil)
	h.mustCall(200, "POST", "/_mock/domains/acme.localhost/dns", map[string]any{"valid": false})
	if e := next(t, ch).body["event"]; e != "domain_dns_proxy_record_invalid" {
		t.Errorf("after flip: %v", e)
	}

	h.mustCall(200, "DELETE", base+"/upstreams/upstream_local", nil)
	if e := next(t, ch).body["event"]; e != "domain_deleted" {
		t.Errorf("cascade delete: %v", e)
	}
	if e := next(t, ch).body["event"]; e != "upstream_deleted" {
		t.Errorf("upstream delete: %v", e)
	}
	select {
	case d := <-ch:
		t.Errorf("unexpected extra webhook %v", d.body["event"])
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWebhookRetries(t *testing.T) {
	recv, ch := webhookReceiver(t, 2)
	h := newHarness(t, func(c *Config) { c.WebhookURL = recv.URL })
	h.mustCall(201, "POST", base+"/upstreams", map[string]any{"host": "retry.example.com"})
	if d := next(t, ch); d.body["event"] != "upstream_created" || d.query.Get("token") != "local_webhook_token" {
		t.Errorf("delivery after retries: %v", d)
	}
}

func TestWebhookDoesNotBlock(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
	}))
	t.Cleanup(slow.Close)
	h := newHarness(t, func(c *Config) { c.WebhookURL = slow.URL })
	start := time.Now()
	h.mustCall(201, "POST", base+"/upstreams", map[string]any{"host": "fast.example.com"})
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("API response took %s while webhook receiver was slow", d)
	}
}

func sameKeys(got, want []string) bool {
	got, want = slices.Clone(got), slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}
