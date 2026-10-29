package mock

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"testing"
	"time"
)

var (
	domainUUIDRe   = regexp.MustCompile(`^domain_[0-9a-f]{8}$`)
	upstreamUUIDRe = regexp.MustCompile(`^upstream_[0-9a-f]{8}$`)
	apiTimeRe      = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
)

func TestAuth(t *testing.T) {
	h := newHarness(t, nil)
	path := base + "/upstreams"
	for _, auth := range []string{"", "Bearer wrong", "scd_test_token", "Basic scd_test_token"} {
		status, body := h.raw("GET", path, nil, auth)
		if status != 401 || len(body) != 0 {
			t.Errorf("auth %q: got %d %q, want 401 with empty body", auth, status, body)
		}
	}
	if status, _ := h.raw("GET", path, nil, "Bearer "+token); status != 200 {
		t.Errorf("valid token: got %d", status)
	}
	if status, _ := h.raw("GET", path, nil, "bearer "+token); status != 200 {
		t.Errorf("lowercase scheme: got %d", status)
	}
	// Auth is checked before the account lookup.
	if status, _ := h.raw("GET", "/api/v1/accounts/nope/upstreams", nil, ""); status != 401 {
		t.Errorf("unknown account without token: got %d, want 401", status)
	}
}

func TestUnknownResources(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		path, msg, code string
	}{
		{"/api/v1/accounts/acc_nope/upstreams", "Account acc_nope not found.", ""},
		{base + "/upstreams/upstream_nope", "Upstream upstream_nope not found.", "not_found"},
		{base + "/upstreams/upstream_nope/custom_domains", "Upstream upstream_nope not found.", "not_found"},
		{domains + "/domain_nope", "Custom domain domain_nope not found.", "not_found"},
	}
	for _, c := range cases {
		status, m := h.call("GET", c.path, nil)
		if status != 404 || m["error"] != c.msg {
			t.Errorf("GET %s: got %d %v", c.path, status, m)
		}
		if code, _ := m["error_code"].(string); code != c.code {
			t.Errorf("GET %s: error_code %q, want %q", c.path, code, c.code)
		}
	}
}

func TestSeededUpstream(t *testing.T) {
	h := newHarness(t, nil)
	m := h.mustCall(200, "GET", base+"/upstreams/upstream_local", nil)
	want := map[string]any{
		"uuid": "upstream_local", "host": "localhost", "port": float64(3000), "tls": false,
		"auth_token": "local_proxy_token", "bubble_io": false, "compression_enabled": true,
		"geocoding_enabled": false, "account_uuid": "acc_local",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
}

func TestUpstreamCRUD(t *testing.T) {
	h := newHarness(t, nil)
	up := h.mustCall(201, "POST", base+"/upstreams", map[string]any{"host": " App.MySaaS.com. "})
	uuid, _ := up["uuid"].(string)
	if !upstreamUUIDRe.MatchString(uuid) {
		t.Errorf("uuid %q does not look like upstream_xxxxxxxx", uuid)
	}
	if up["host"] != "app.mysaas.com" || up["port"] != float64(443) || up["tls"] != true ||
		up["compression_enabled"] != true || up["geocoding_enabled"] != false || up["bubble_io"] != false {
		t.Errorf("defaults wrong: %v", up)
	}
	if tok, _ := up["auth_token"].(string); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(tok) {
		t.Errorf("generated auth_token %q, want 32 hex chars", tok)
	}
	for _, k := range []string{"created_at", "updated_at"} {
		if s, _ := up[k].(string); !apiTimeRe.MatchString(s) {
			t.Errorf("%s = %q, want ISO 8601 with milliseconds", k, s)
		}
	}

	// Form-encoded bodies work too, like the curl examples in the docs.
	form := url.Values{"host": {"api.mysaas.com"}, "port": {"8888"}, "tls": {"false"}, "auth_token": {"my-token_1"}}
	up2 := h.mustCall(201, "POST", base+"/upstreams", form)
	if up2["port"] != float64(8888) || up2["tls"] != false || up2["auth_token"] != "my-token_1" {
		t.Errorf("form create: %v", up2)
	}

	if got := h.mustCall(200, "GET", base+"/upstreams/"+uuid, nil); got["host"] != "app.mysaas.com" {
		t.Errorf("retrieve: %v", got)
	}
	list := h.mustCall(200, "GET", base+"/upstreams?host=api.mysaas.com", nil)
	if data := list["data"].([]any); len(data) != 1 || data[0].(map[string]any)["uuid"] != up2["uuid"] {
		t.Errorf("host filter: %v", list)
	}

	// Deleting an upstream deletes its custom domains.
	h.mustCall(201, "POST", base+"/upstreams/"+uuid+"/custom_domains", map[string]any{"host": "x.example.com"})
	del := h.mustCall(200, "DELETE", base+"/upstreams/"+uuid, nil)
	if del["message"] != fmt.Sprintf("Upstream %s (app.mysaas.com) deleted.", uuid) {
		t.Errorf("delete message: %v", del)
	}
	h.mustCall(404, "GET", base+"/upstreams/"+uuid, nil)
	h.createDomain("x.example.com", nil) // host is free again
}

func TestUpstreamValidation(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		body map[string]any
		code string
	}{
		{map[string]any{}, "host_blank"},
		{map[string]any{"host": "  "}, "host_blank"},
		{map[string]any{"host": "bad host"}, "host_invalid"},
		{map[string]any{"host": "*.wild.com"}, "host_invalid"},
		{map[string]any{"host": "ok.com", "port": 0}, "port_invalid"},
		{map[string]any{"host": "ok.com", "port": "abc"}, "port_invalid"},
		{map[string]any{"host": "ok.com", "tls": "maybe"}, "tls_invalid"},
		{map[string]any{"host": "ok.com", "auth_token": "has space"}, "auth_token_invalid"},
	}
	for _, c := range cases {
		status, m := h.call("POST", base+"/upstreams", c.body)
		if status != 400 || m["error_code"] != c.code || m["error"] == "" {
			t.Errorf("%v: got %d %v, want 400 %s", c.body, status, m, c.code)
		}
	}
	// Single-label hosts (docker service names) are allowed for upstreams.
	h.mustCall(201, "POST", base+"/upstreams", map[string]any{"host": "app", "port": 3000})

	status, m := h.call("POST", base+"/upstreams", "{not json")
	if status != 400 || m["error_code"] != "bad_request" {
		t.Errorf("bad JSON: got %d %v", status, m)
	}
}

func TestCustomDomainShape(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DNSDelay = time.Hour })
	d := h.createDomain("App.User.com.", map[string]any{"prepend_path": "/sites/client1/"})
	want := []string{
		"uuid", "host", "prepend_path", "bubble_target_path", "meta_title", "meta_description",
		"meta_favicon_url", "meta_image_url", "created_at", "updated_at", "last_dns_check_at",
		"status", "tls_certificate_issued", "acme_challenge_dns_record_status", "challenge_type",
		"redirect_to_www", "instructions_recipient", "instructions_email_sent_at", "upstream_uuid",
		"delegated_domain_control_validation_record_hostname",
		"delegated_domain_control_validation_record_value",
	}
	got := keys(d)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("keys = %v\nwant %v", got, want)
	}
	if !domainUUIDRe.MatchString(d["uuid"].(string)) {
		t.Errorf("uuid %v", d["uuid"])
	}
	checks := map[string]any{
		"host": "app.user.com", "prepend_path": "sites/client1", "status": "pending_check",
		"tls_certificate_issued": false, "last_dns_check_at": nil, "challenge_type": "http01",
		"acme_challenge_dns_record_status": "valid", "redirect_to_www": false,
		"upstream_uuid": "upstream_local", "meta_title": nil,
		"delegated_domain_control_validation_record_hostname": nil,
	}
	for k, v := range checks {
		if d[k] != v {
			t.Errorf("%s = %v, want %v", k, d[k], v)
		}
	}

	dns01 := h.createDomain("shop.user.com", map[string]any{"challenge_type": "dns01", "instructions_recipient": "a@b.co"})
	if dns01["acme_challenge_dns_record_status"] != "pending" ||
		dns01["delegated_domain_control_validation_record_hostname"] != "_acme-challenge.shop.user.com" ||
		dns01["instructions_recipient"] != "a@b.co" {
		t.Errorf("dns01 domain: %v", dns01)
	}

	if got := h.getDomain(d["uuid"].(string)); got["host"] != "app.user.com" {
		t.Errorf("retrieve: %v", got)
	}
	purge := h.mustCall(202, "POST", domains+"/"+d["uuid"].(string)+"/purge_http_cache", nil)
	if purge["message"] != "HTTP cache purge initiated for domain app.user.com." {
		t.Errorf("purge: %v", purge)
	}
	del := h.mustCall(200, "DELETE", domains+"/"+d["uuid"].(string), nil)
	if del["message"] != fmt.Sprintf("Custom domain %s (app.user.com) deleted.", d["uuid"]) {
		t.Errorf("delete: %v", del)
	}
	h.mustCall(404, "GET", domains+"/"+d["uuid"].(string), nil)
}

func TestCustomDomainValidation(t *testing.T) {
	h := newHarness(t, nil)
	h.createDomain("taken.example.com", nil)
	cases := []struct {
		body map[string]any
		code string
	}{
		{map[string]any{}, "host_blank"},
		{map[string]any{"host": ""}, "host_blank"},
		{map[string]any{"host": "TAKEN.example.com."}, "host_taken"},
		{map[string]any{"host": "nodot"}, "host_invalid"},
		{map[string]any{"host": "-bad.example.com"}, "host_invalid"},
		{map[string]any{"host": "has space.com"}, "host_invalid"},
		{map[string]any{"host": "https://x.com"}, "host_invalid"},
		{map[string]any{"host": "*.wild.example.com"}, "host_invalid"},
		{map[string]any{"host": 42}, "host_invalid"},
		{map[string]any{"host": []int{1}}, "host_invalid"},
		{map[string]any{"host": "ok.example.com", "prepend_path": "a b"}, "prepend_path_invalid"},
		{map[string]any{"host": "ok.example.com", "challenge_type": "tls01"}, "challenge_type_invalid"},
		{map[string]any{"host": "ok.example.com", "instructions_recipient": "nope"}, "instructions_recipient_invalid"},
		{map[string]any{"host": "ok.example.com", "redirect_to_www": "sure"}, "redirect_to_www_invalid"},
	}
	for _, c := range cases {
		status, m := h.call("POST", domains, c.body)
		if status != 400 || m["error_code"] != c.code {
			t.Errorf("%v: got %d %v, want 400 %s", c.body, status, m, c.code)
		}
	}
	_, m := h.call("POST", domains, map[string]any{"host": "taken.example.com"})
	if m["error"] != "Host has already been taken" {
		t.Errorf("taken message: %v", m["error"])
	}
}

func TestPagination(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DNSDelay = time.Hour })
	for i := 1; i <= 25; i++ {
		h.createDomain(fmt.Sprintf("d%02d.example.com", i), nil)
	}
	cases := []struct {
		query     string
		page, n   int
		firstHost string
		wantCount int
	}{
		{"", 1, 20, "d01.example.com", 25},
		{"?page=2", 2, 5, "d21.example.com", 25},
		{"?per_page=10&page=3", 3, 5, "d21.example.com", 25},
		{"?per_page=10&page=99", 3, 5, "d21.example.com", 25}, // overflow returns the last page
		{"?page=0", 1, 20, "d01.example.com", 25},
		{"?per_page=500", 1, 25, "d01.example.com", 25}, // capped at 100
		{"?host=D07.example.com", 1, 1, "d07.example.com", 1},
	}
	for _, c := range cases {
		m := h.mustCall(200, "GET", domains+c.query, nil)
		data := m["data"].([]any)
		pg := m["pagination"].(map[string]any)
		if len(data) != c.n || pg["page"] != float64(c.page) || pg["count"] != float64(c.wantCount) {
			t.Errorf("%q: %d items, pagination %v", c.query, len(data), pg)
			continue
		}
		if host := data[0].(map[string]any)["host"]; host != c.firstHost {
			t.Errorf("%q: first host %v, want %s", c.query, host, c.firstHost)
		}
	}
	empty := h.mustCall(200, "GET", domains+"?host=missing.example.com", nil)
	if data := empty["data"].([]any); len(data) != 0 || empty["pagination"].(map[string]any)["page"] != float64(1) {
		t.Errorf("empty list: %v", empty)
	}
}

func TestDNSTransitions(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ValidHosts = []string{"*.example.com", "exact.test"} })
	cases := map[string]string{
		"acme.localhost":    "dns_records_found",
		"a.b.example.com":   "dns_records_found",
		"exact.test":        "dns_records_found",
		"example.com":       "dns_records_not_found", // wildcard does not match the apex
		"other.example.org": "dns_records_not_found",
	}
	uuids := map[string]string{}
	for host := range cases {
		d := h.createDomain(host, nil)
		if d["status"] != "pending_check" {
			t.Errorf("%s starts as %v", host, d["status"])
		}
		uuids[host] = d["uuid"].(string)
	}
	for host, want := range cases {
		eventually(t, host+" to become "+want, func() bool { return h.getDomain(uuids[host])["status"] == want })
		d := h.getDomain(uuids[host])
		if d["tls_certificate_issued"] != (want == "dns_records_found") || d["last_dns_check_at"] == nil {
			t.Errorf("%s: %v", host, d)
		}
	}

	verify := func(host string) map[string]any {
		return h.mustCall(200, "POST", domains+"/"+uuids[host]+"/verify_dns_records", nil)
	}
	if v := verify("acme.localhost"); v["dns_status"] != "valid" || v["host"] != "acme.localhost" ||
		v["message"] != "DNS records verification complete." {
		t.Errorf("verify valid: %v", v)
	}
	if v := verify("other.example.org"); v["dns_status"] != "invalid" {
		t.Errorf("verify invalid: %v", v)
	}

	// The control endpoint flips a domain and the override sticks for verify.
	h.mustCall(200, "POST", "/_mock/domains/other.example.org/dns", map[string]any{"valid": true})
	if v := verify("other.example.org"); v["dns_status"] != "valid" {
		t.Errorf("after flip to valid: %v", v)
	}
	d := h.mustCall(200, "POST", "/_mock/domains/ACME.localhost/dns", map[string]any{"valid": false})
	if d["status"] != "dns_records_not_found" || d["tls_certificate_issued"] != true {
		t.Errorf("after flip to invalid: %v", d)
	}
	if v := verify("acme.localhost"); v["dns_status"] != "invalid" {
		t.Errorf("verify after flip: %v", v)
	}
}

func TestVerifyBeforeDelay(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DNSDelay = time.Hour })
	d := h.createDomain("now.localhost", nil)
	v := h.mustCall(200, "POST", domains+"/"+d["uuid"].(string)+"/verify_dns_records", nil)
	if v["dns_status"] != "valid" {
		t.Errorf("verify runs the check immediately: %v", v)
	}
	if dnsStatus(statusPendingCheck) != "pending" || dnsStatus("dns_check_unnecessary") != "valid" {
		t.Error("dnsStatus mapping")
	}
}

func TestMockEndpoints(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Domains = []string{"seeded.example.com", "Seeded.example.com"} })

	status, body := h.raw("GET", "/_mock/health", nil, "")
	if status != 200 || string(body) != "{\"status\":\"ok\"}\n" {
		t.Errorf("health: %d %s", status, body)
	}

	state := h.mustCall(200, "GET", "/_mock/state", nil)
	doms := state["custom_domains"].([]any)
	if len(doms) != 1 || doms[0].(map[string]any)["status"] != "dns_records_found" ||
		doms[0].(map[string]any)["tls_certificate_issued"] != true {
		t.Fatalf("seeded domains: %v", doms)
	}
	// Seeded domains stay valid on verify even though the host rules say no.
	uuid := doms[0].(map[string]any)["uuid"].(string)
	if v := h.mustCall(200, "POST", domains+"/"+uuid+"/verify_dns_records", nil); v["dns_status"] != "valid" {
		t.Errorf("seeded verify: %v", v)
	}

	h.createDomain("extra.localhost", nil)
	h.mustCall(201, "POST", base+"/upstreams", map[string]any{"host": "x.example.com"})
	if m := h.mustCall(200, "POST", "/_mock/reset", nil); m["message"] != "State reset." {
		t.Errorf("reset: %v", m)
	}
	state = h.mustCall(200, "GET", "/_mock/state", nil)
	if len(state["custom_domains"].([]any)) != 1 || len(state["upstreams"].([]any)) != 1 {
		t.Errorf("state after reset: %v", state)
	}

	if status, m := h.call("POST", "/_mock/domains/missing.localhost/dns", map[string]any{"valid": true}); status != 404 || m["error_code"] != "not_found" {
		t.Errorf("unknown host: %d %v", status, m)
	}
	if status, m := h.call("POST", "/_mock/domains/seeded.example.com/dns", map[string]any{}); status != 400 || m["error_code"] != "valid_blank" {
		t.Errorf("missing valid: %d %v", status, m)
	}
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAddr != ":4010" || cfg.ProxyAddr != ":4080" || cfg.UpstreamPort != 3000 ||
		cfg.DNSDelay != 2*time.Second || cfg.WebhookToken != "local_webhook_token" || cfg.UpstreamTLS {
		t.Errorf("defaults: %+v", cfg)
	}
	env := map[string]string{
		"SCD_MOCK_DNS_DELAY": "0.5", "SCD_MOCK_DOMAINS": " a.localhost, b.localhost ,",
		"SCD_MOCK_UPSTREAM_TLS": "true", "SCD_MOCK_FORWARD_HOST": "1",
	}
	cfg, err = ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || cfg.DNSDelay != 500*time.Millisecond || len(cfg.Domains) != 2 || !cfg.UpstreamTLS || !cfg.ForwardHost {
		t.Errorf("parsed: %+v %v", cfg, err)
	}
	bad := map[string]string{"SCD_MOCK_UPSTREAM_PORT": "70000", "SCD_MOCK_DNS_DELAY": "soon",
		"SCD_MOCK_KEEP_HOST": "yes please", "SCD_MOCK_WEBHOOK_URL": "not a url"}
	if _, err := ConfigFromEnv(func(k string) string { return bad[k] }); err == nil {
		t.Error("expected an error for invalid env")
	}
}
