package mock

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

type routeKey struct{}

// route is what the proxy needs to forward one request, copied out of the
// state so the lock is not held while proxying.
type route struct {
	requestHost string // custom domain hostname, no port
	upstream    Upstream
	prependPath string
}

// Headers production sets on every forwarded request, overwriting anything
// the client sent.
var managedHeaders = []string{
	"X-Served-For", "X-SaaS-Domains-IP", "X-SaaS-Domains-Auth-Token",
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
}

// ProxyHandler forwards requests for known, DNS-valid custom domains to their
// upstream.
func (s *Server) ProxyHandler() http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: s.rewrite,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Printf("[proxy] upstream error for %s: %v", r.Host, err)
			http.Error(w, "Bad gateway: the upstream for this custom domain is not reachable", http.StatusBadGateway)
		},
	}
	return s.logRequests("[proxy]", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := requestHostname(r.Host)
		s.mu.Lock()
		d := s.domainByHost(host)
		var rt route
		var status string
		if d != nil {
			status = d.Status
			rt = route{requestHost: host, upstream: *s.findUpstream(d.UpstreamUUID)}
			if d.PrependPath != nil {
				rt.prependPath = *d.PrependPath
			}
		}
		s.mu.Unlock()

		switch {
		case d == nil:
			http.Error(w, "Unknown custom domain", http.StatusNotFound)
		case status != statusFound:
			http.Error(w, fmt.Sprintf("Custom domain %s is not active yet (status: %s). "+
				"Wait for the simulated DNS check, call verify_dns_records, or "+
				`POST /_mock/domains/%s/dns with {"valid": true}.`, host, status, host),
				http.StatusMisdirectedRequest)
		default:
			rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, rt)))
		}
	}))
}

func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	rt := pr.In.Context().Value(routeKey{}).(route)
	up := rt.upstream
	scheme := "http"
	if up.TLS {
		scheme = "https"
	}
	pr.SetURL(&url.URL{Scheme: scheme, Host: net.JoinHostPort(up.Host, strconv.Itoa(up.Port))})
	if rt.prependPath != "" {
		prefix := "/" + rt.prependPath
		pr.Out.URL.Path = prefix + pr.In.URL.Path
		pr.Out.URL.RawPath = ""
		if pr.In.URL.RawPath != "" {
			pr.Out.URL.RawPath = prefix + pr.In.URL.RawPath
		}
	}

	upstreamHost := hostHeader(up.Host, up.Port, up.TLS)
	pr.Out.Host = upstreamHost
	if s.cfg.KeepHost {
		pr.Out.Host = pr.In.Host
	}

	clientIP := pr.In.RemoteAddr
	if h, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = h
	}
	h := pr.Out.Header
	for _, k := range managedHeaders {
		h.Del(k)
	}
	h.Set("X-Served-For", rt.requestHost)
	h.Set("X-SaaS-Domains-IP", clientIP)
	h.Set("X-SaaS-Domains-Auth-Token", up.AuthToken)
	h.Set("X-Forwarded-Proto", s.cfg.ForwardedProto)
	h.Set("X-Forwarded-For", clientIP)
	h.Set("X-Forwarded-Host", upstreamHost)
	if s.cfg.ForwardHost {
		h.Set("X-Forwarded-Host", rt.requestHost)
	}
}

// hostHeader returns host, plus :port when the port is not the scheme default.
func hostHeader(host string, port int, tls bool) string {
	if (tls && port == 443) || (!tls && port == 80) {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// requestHostname strips the port from a Host header and normalizes it.
func requestHostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		hostport = h
	}
	return normalizeHost(strings.Trim(hostport, "[]"))
}
