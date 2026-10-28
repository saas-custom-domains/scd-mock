package mock

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type seen struct {
	host, uri string
	header    http.Header
}

// echoUpstream records what the proxy forwarded.
func echoUpstream(t *testing.T) (*httptest.Server, chan seen) {
	ch := make(chan seen, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- seen{host: r.Host, uri: r.URL.RequestURI(), header: r.Header.Clone()}
		fmt.Fprint(w, "hello from upstream")
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// proxyHarness seeds the default upstream to point at a test server.
func proxyHarness(t *testing.T, upstream *httptest.Server, mutate func(*Config)) *harness {
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())
	return newHarness(t, func(c *Config) {
		c.UpstreamHost, c.UpstreamPort = "127.0.0.1", port
		c.Domains = []string{"acme.localhost"}
		if mutate != nil {
			mutate(c)
		}
	})
}

func (h *harness) viaProxy(host, path string, header http.Header) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.proxy.URL+path, nil)
	req.Host = host
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func receive(t *testing.T, ch chan seen) seen {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("upstream received nothing")
		return seen{}
	}
}

func TestProxyHeaders(t *testing.T) {
	up, ch := echoUpstream(t)
	h := proxyHarness(t, up, nil)
	upstreamHost := strings.TrimPrefix(up.URL, "http://")

	spoofed := http.Header{
		"X-Served-For":              {"victim.example.com"},
		"X-Saas-Domains-Ip":         {"6.6.6.6"},
		"X-Saas-Domains-Auth-Token": {"forged"},
		"X-Forwarded-For":           {"1.2.3.4"},
		"X-Forwarded-Host":          {"evil.example.com"},
		"X-Forwarded-Proto":         {"gopher"},
		"X-Custom":                  {"kept"},
	}
	status, body := h.viaProxy("ACME.localhost:4080", "/hello?x=1", spoofed)
	if status != 200 || body != "hello from upstream" {
		t.Fatalf("got %d %q", status, body)
	}
	got := receive(t, ch)
	if got.host != upstreamHost || got.uri != "/hello?x=1" {
		t.Errorf("host %q uri %q", got.host, got.uri)
	}
	want := map[string]string{
		"X-Served-For":              "acme.localhost",
		"X-Saas-Domains-Ip":         "127.0.0.1",
		"X-Saas-Domains-Auth-Token": "local_proxy_token",
		"X-Forwarded-For":           "127.0.0.1",
		"X-Forwarded-Host":          upstreamHost,
		"X-Forwarded-Proto":         "https",
		"X-Custom":                  "kept",
	}
	for k, v := range want {
		if vals := got.header.Values(k); len(vals) != 1 || vals[0] != v {
			t.Errorf("%s = %q, want [%q]", k, vals, v)
		}
	}
}

func TestProxyHostFlags(t *testing.T) {
	up, ch := echoUpstream(t)
	h := proxyHarness(t, up, func(c *Config) { c.ForwardHost, c.KeepHost = true, true })
	h.viaProxy("acme.localhost:4080", "/", nil)
	got := receive(t, ch)
	if got.host != "acme.localhost:4080" {
		t.Errorf("SCD_MOCK_KEEP_HOST: Host = %q", got.host)
	}
	if v := got.header.Get("X-Forwarded-Host"); v != "acme.localhost" {
		t.Errorf("SCD_MOCK_FORWARD_HOST: X-Forwarded-Host = %q", v)
	}
}

func TestProxyPrependPath(t *testing.T) {
	up, ch := echoUpstream(t)
	h := proxyHarness(t, up, nil)
	d := h.createDomain("tenant.localhost", map[string]any{"prepend_path": "/sites/client1"})
	h.mustCall(200, "POST", domains+"/"+d["uuid"].(string)+"/verify_dns_records", nil)

	for path, want := range map[string]string{
		"/about?x=1":   "/sites/client1/about?x=1",
		"/":            "/sites/client1/",
		"/a%2Fb/c%20d": "/sites/client1/a%2Fb/c%20d",
	} {
		if status, _ := h.viaProxy("tenant.localhost", path, nil); status != 200 {
			t.Fatalf("%s: status %d", path, status)
		}
		if got := receive(t, ch); got.uri != want {
			t.Errorf("%s forwarded as %q, want %q", path, got.uri, want)
		}
	}
}

func TestProxyUnknownAndInactive(t *testing.T) {
	up, ch := echoUpstream(t)
	h := proxyHarness(t, up, func(c *Config) { c.DNSDelay = time.Hour })

	status, body := h.viaProxy("nobody.localhost", "/", nil)
	if status != 404 || body != "Unknown custom domain\n" {
		t.Errorf("unknown: %d %q", status, body)
	}

	h.createDomain("pending.localhost", nil)
	status, body = h.viaProxy("pending.localhost:4080", "/", nil)
	if status != 421 || !strings.Contains(body, "not active yet (status: pending_check)") {
		t.Errorf("pending: %d %q", status, body)
	}

	h.mustCall(200, "POST", "/_mock/domains/acme.localhost/dns", map[string]any{"valid": false})
	if status, _ = h.viaProxy("acme.localhost", "/", nil); status != 421 {
		t.Errorf("flipped invalid: %d", status)
	}
	select {
	case s := <-ch:
		t.Errorf("upstream should not be reached, got %v", s)
	default:
	}
}

func TestProxyUpstreamDown(t *testing.T) {
	up, _ := echoUpstream(t)
	h := proxyHarness(t, up, nil)
	up.Close()
	if status, _ := h.viaProxy("acme.localhost", "/", nil); status != 502 {
		t.Errorf("got %d, want 502", status)
	}
}

func TestProxyUpgrade(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" || r.Header.Get("X-Served-For") != "acme.localhost" {
			http.Error(w, "bad upgrade", 400)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		io.Copy(conn, brw) // echo
	}))
	t.Cleanup(up.Close)
	h := proxyHarness(t, up, nil)

	conn, err := net.Dial("tcp", strings.TrimPrefix(h.proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: acme.localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade response: %v %v", resp, err)
	}
	fmt.Fprint(conn, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Errorf("echo: %q %v", buf, err)
	}
}

func TestHostHelpers(t *testing.T) {
	for _, c := range []struct {
		host string
		port int
		tls  bool
		want string
	}{
		{"localhost", 80, false, "localhost"},
		{"localhost", 3000, false, "localhost:3000"},
		{"app.com", 443, true, "app.com"},
		{"app.com", 443, false, "app.com:443"},
	} {
		if got := hostHeader(c.host, c.port, c.tls); got != c.want {
			t.Errorf("hostHeader(%s, %d, %t) = %s", c.host, c.port, c.tls, got)
		}
	}
	for in, want := range map[string]string{"Acme.Localhost:4080": "acme.localhost", "a.com.": "a.com", "a.com": "a.com"} {
		if got := requestHostname(in); got != want {
			t.Errorf("requestHostname(%q) = %q", in, got)
		}
	}
}
