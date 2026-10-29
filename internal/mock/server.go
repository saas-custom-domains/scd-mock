package mock

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server holds the in-memory state shared by the API and proxy listeners.
type Server struct {
	cfg   Config
	log   *log.Logger
	hooks *webhooks
	now   func() time.Time

	mu        sync.Mutex
	upstreams []*Upstream // creation order
	domains   []*Domain   // creation order
	gen       int         // bumped on reset so stale DNS timers do nothing
}

// New validates the seed configuration and returns a ready Server.
func New(cfg Config, logger *log.Logger) (*Server, error) {
	s := &Server{cfg: cfg, log: logger, now: time.Now}
	s.hooks = newWebhooks(cfg, logger)
	ups, doms, err := s.seedState()
	if err != nil {
		return nil, err
	}
	s.upstreams, s.domains = ups, doms
	return s, nil
}

// seedState builds the initial upstream and domains from config.
func (s *Server) seedState() ([]*Upstream, []*Domain, error) {
	now := s.now()
	up := &Upstream{
		UUID: s.cfg.UpstreamUUID, AccountUUID: s.cfg.AccountUUID,
		Host: normalizeHost(s.cfg.UpstreamHost), Port: s.cfg.UpstreamPort, TLS: s.cfg.UpstreamTLS,
		AuthToken: s.cfg.UpstreamAuthToken, CompressionEnabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if !validHostname(up.Host) {
		return nil, nil, fmt.Errorf("SCD_MOCK_UPSTREAM_HOST %q is not a valid hostname", s.cfg.UpstreamHost)
	}
	if !authTokenRe.MatchString(up.AuthToken) {
		return nil, nil, fmt.Errorf("SCD_MOCK_UPSTREAM_AUTH_TOKEN must be 1 to 64 letters, digits, dashes or underscores")
	}

	var domains []*Domain
	for _, h := range s.cfg.Domains {
		host := normalizeHost(h)
		if err := validateDomainHost(host); err != nil {
			return nil, nil, fmt.Errorf("SCD_MOCK_DOMAINS: %s", err.Message)
		}
		if slices.ContainsFunc(domains, func(d *Domain) bool { return d.Host == host }) {
			continue
		}
		d := newDomain(up.UUID, host, now)
		valid := true
		d.dnsOverride = &valid
		d.applyDNS(true, now)
		domains = append(domains, d)
	}
	return []*Upstream{up}, domains, nil
}

func newDomain(upstreamUUID, host string, now time.Time) *Domain {
	return &Domain{
		UUID: newUUID("domain_"), UpstreamUUID: upstreamUUID, Host: host,
		ChallengeType: challengeHTTP01, Status: statusPendingCheck, AcmeStatus: acmeValid,
		CreatedAt: now, UpdatedAt: now,
	}
}

// --- lookups (callers hold s.mu) ---

func (s *Server) findUpstream(uuid string) *Upstream {
	for _, u := range s.upstreams {
		if u.UUID == uuid {
			return u
		}
	}
	return nil
}

func (s *Server) domainByHost(host string) *Domain {
	for _, d := range s.domains {
		if d.Host == host {
			return d
		}
	}
	return nil
}

func (s *Server) dnsValid(d *Domain) bool {
	if d.dnsOverride != nil {
		return *d.dnsOverride
	}
	return hostRuleValid(d.Host, s.cfg.ValidHosts)
}

// resolve finds the upstream and domain named in the request path, returning
// production-shaped 404 errors.
func (s *Server) resolve(r *http.Request) (*Upstream, *Domain, *apiError) {
	if acc := r.PathValue("account"); acc != s.cfg.AccountUUID {
		return nil, nil, &apiError{Status: 404, Message: fmt.Sprintf("Account %s not found.", acc)}
	}
	upUUID := r.PathValue("upstream")
	if upUUID == "" {
		return nil, nil, nil
	}
	up := s.findUpstream(upUUID)
	if up == nil {
		return nil, nil, &apiError{Status: 404, Message: fmt.Sprintf("Upstream %s not found.", upUUID), Code: "not_found"}
	}
	domUUID := r.PathValue("domain")
	if domUUID == "" {
		return up, nil, nil
	}
	for _, d := range s.domains {
		if d.UUID == domUUID && d.UpstreamUUID == up.UUID {
			return up, d, nil
		}
	}
	return nil, nil, &apiError{Status: 404, Message: fmt.Sprintf("Custom domain %s not found.", domUUID), Code: "not_found"}
}

// --- routing ---

// APIHandler serves the REST API and the /_mock control endpoints.
func (s *Server) APIHandler() http.Handler {
	api := http.NewServeMux()
	const acc = "/api/v1/accounts/{account}"
	const doms = acc + "/upstreams/{upstream}/custom_domains"
	api.Handle("GET "+acc+"/upstreams", s.handle(s.listUpstreams))
	api.Handle("POST "+acc+"/upstreams", s.handle(s.createUpstream))
	api.Handle("GET "+acc+"/upstreams/{upstream}", s.handle(s.showUpstream))
	api.Handle("DELETE "+acc+"/upstreams/{upstream}", s.handle(s.deleteUpstream))
	api.Handle("GET "+doms, s.handle(s.listDomains))
	api.Handle("POST "+doms, s.handle(s.createDomain))
	api.Handle("GET "+doms+"/{domain}", s.handle(s.showDomain))
	api.Handle("DELETE "+doms+"/{domain}", s.handle(s.deleteDomain))
	api.Handle("POST "+doms+"/{domain}/verify_dns_records", s.handle(s.verifyDomain))
	api.Handle("POST "+doms+"/{domain}/purge_http_cache", s.handle(s.purgeDomain))

	mux := http.NewServeMux()
	mux.Handle("/api/", s.requireToken(api))
	mux.Handle("POST /_mock/domains/{host}/dns", s.handle(s.mockSetDNS))
	mux.Handle("POST /_mock/reset", s.handle(s.mockReset))
	mux.Handle("GET /_mock/state", s.handle(s.mockState))
	mux.HandleFunc("GET /_mock/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return s.logRequests("[api]", mux)
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	want := []byte(s.cfg.APIToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), want) != 1 {
			w.WriteHeader(http.StatusUnauthorized) // production sends an empty body
			return
		}
		next.ServeHTTP(w, r)
	})
}

// result is what an API action produces: a response plus webhook events.
type result struct {
	status int
	body   any
	events []event
}

func fail(e *apiError) result { return result{status: e.Status, body: e} }

// handle parses params, runs fn under the state lock, queues its webhooks
// and writes the response.
func (s *Server) handle(fn func(*http.Request, params) result) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, perr := readParams(r)
		if perr != nil {
			writeJSON(w, perr.Status, perr)
			return
		}
		s.mu.Lock()
		res := fn(r, p)
		s.hooks.send(res.events...) // non-blocking; under the lock to keep event order
		s.mu.Unlock()
		writeJSON(w, res.status, res.body)
	})
}

// --- upstreams ---

func (s *Server) listUpstreams(r *http.Request, p params) result {
	if _, _, err := s.resolve(r); err != nil {
		return fail(err)
	}
	items := s.upstreams
	if h := r.URL.Query().Get("host"); h != "" {
		items = filter(items, func(u *Upstream) bool { return u.Host == normalizeHost(h) })
	}
	return result{status: 200, body: paginate(items, r.URL.Query(), (*Upstream).JSON)}
}

func (s *Server) showUpstream(r *http.Request, p params) result {
	up, _, err := s.resolve(r)
	if err != nil {
		return fail(err)
	}
	return result{status: 200, body: up.JSON()}
}

func (s *Server) createUpstream(r *http.Request, p params) result {
	if _, _, err := s.resolve(r); err != nil {
		return fail(err)
	}
	host, err := p.str("host")
	if err != nil {
		return fail(err)
	}
	now := s.now()
	up := &Upstream{UUID: newUUID("upstream_"), AccountUUID: s.cfg.AccountUUID, CreatedAt: now, UpdatedAt: now}
	if host == nil || strings.TrimSpace(*host) == "" {
		return fail(badRequest("host_blank", "Host can't be blank"))
	}
	up.Host = normalizeHost(*host)
	if !validHostname(up.Host) {
		return fail(badRequest("host_invalid", "Host %s is invalid. Please use something like app.domain.com or domain.com", up.Host))
	}
	if up.Port, err = p.integer("port", defaultUpstreamPort); err != nil {
		return fail(err)
	}
	if up.Port < 1 || up.Port > 65535 {
		return fail(badRequest("port_invalid", "Port must be between 1 and 65535"))
	}
	for _, b := range []struct {
		key string
		dst *bool
		def bool
	}{
		{"tls", &up.TLS, true},
		{"bubble_io", &up.BubbleIO, false},
		{"compression_enabled", &up.CompressionEnabled, true},
		{"geocoding_enabled", &up.GeocodingEnabled, false},
	} {
		if *b.dst, err = p.boolean(b.key, b.def); err != nil {
			return fail(err)
		}
	}
	token, err := p.str("auth_token")
	if err != nil {
		return fail(err)
	}
	up.AuthToken = randomHex(16)
	if token != nil && *token != "" {
		if !authTokenRe.MatchString(*token) {
			return fail(badRequest("auth_token_invalid", "Auth token must be 1 to 64 characters long and only contain alphanumeric characters, dashes, and underscores"))
		}
		up.AuthToken = *token
	}
	s.upstreams = append(s.upstreams, up)
	return result{status: 201, body: up.JSON(), events: []event{up.webhookEvent("upstream_created")}}
}

func (s *Server) deleteUpstream(r *http.Request, p params) result {
	up, _, err := s.resolve(r)
	if err != nil {
		return fail(err)
	}
	var events []event
	s.domains = filter(s.domains, func(d *Domain) bool {
		if d.UpstreamUUID == up.UUID {
			events = append(events, d.webhookEvent("domain_deleted"))
			return false
		}
		return true
	})
	s.upstreams = filter(s.upstreams, func(u *Upstream) bool { return u != up })
	events = append(events, up.webhookEvent("upstream_deleted"))
	msg := fmt.Sprintf("Upstream %s (%s) deleted.", up.UUID, up.Host)
	return result{status: 200, body: map[string]string{"message": msg}, events: events}
}

// --- custom domains ---

func (s *Server) listDomains(r *http.Request, p params) result {
	up, _, err := s.resolve(r)
	if err != nil {
		return fail(err)
	}
	host := normalizeHost(r.URL.Query().Get("host"))
	items := filter(s.domains, func(d *Domain) bool {
		return d.UpstreamUUID == up.UUID && (host == "" || d.Host == host)
	})
	return result{status: 200, body: paginate(items, r.URL.Query(), (*Domain).JSON)}
}

func (s *Server) showDomain(r *http.Request, p params) result {
	_, d, err := s.resolve(r)
	if err != nil {
		return fail(err)
	}
	return result{status: 200, body: d.JSON()}
}

func (s *Server) createDomain(r *http.Request, p params) result {
	up, _, err := s.resolve(r)
	if err != nil {
		return fail(err)
	}
	f, err := p.strs("host", "instructions_recipient", "challenge_type", "prepend_path",
		"bubble_target_path", "meta_title", "meta_description", "meta_favicon_url", "meta_image_url")
	if err != nil {
		return fail(err)
	}

	if f["host"] == nil || strings.TrimSpace(*f["host"]) == "" {
		return fail(badRequest("host_blank", "Host can't be blank"))
	}
	host := normalizeHost(*f["host"])
	if s.domainByHost(host) != nil {
		return fail(badRequest("host_taken", "Host has already been taken"))
	}
	if err := validateDomainHost(host); err != nil {
		return fail(err)
	}
	d := newDomain(up.UUID, host, s.now())
	if rcpt := f["instructions_recipient"]; rcpt != nil && *rcpt != "" {
		if err := validateEmail(rcpt); err != nil {
			return fail(err)
		}
		d.InstructionsRecipient = rcpt
	}
	if ct := f["challenge_type"]; ct != nil && *ct != "" {
		if *ct != challengeHTTP01 && *ct != challengeDNS01 {
			return fail(badRequest("challenge_type_invalid", "Challenge type is not included in the list"))
		}
		d.ChallengeType = *ct
	}
	if d.ChallengeType == challengeDNS01 {
		d.AcmeStatus = acmePending
	}
	for _, m := range []struct {
		key, label string
		dst        **string
	}{
		{"meta_title", "Meta title", &d.MetaTitle},
		{"meta_description", "Meta description", &d.MetaDescription},
		{"meta_favicon_url", "Meta favicon url", &d.MetaFaviconURL},
		{"meta_image_url", "Meta image url", &d.MetaImageURL},
	} {
		if err := validateMeta(m.key, m.label, f[m.key]); err != nil {
			return fail(err)
		}
		*m.dst = f[m.key]
	}
	if d.PrependPath, err = normalizePath("prepend_path", "Prepend path", f["prepend_path"]); err != nil {
		return fail(err)
	}
	if d.BubbleTargetPath, err = normalizePath("bubble_target_path", "Bubble target path", f["bubble_target_path"]); err != nil {
		return fail(err)
	}
	if d.RedirectToWWW, err = p.boolean("redirect_to_www", false); err != nil {
		return fail(err)
	}

	s.domains = append(s.domains, d)
	gen := s.gen
	time.AfterFunc(s.cfg.DNSDelay, func() { s.scheduledCheck(d, gen) })
	return result{status: 201, body: d.JSON(), events: []event{d.webhookEvent("domain_created")}}
}

// scheduledCheck runs the simulated DNS check DNSDelay after creation.
func (s *Server) scheduledCheck(d *Domain, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen == gen && slices.Contains(s.domains, d) {
		s.hooks.send(d.applyDNS(s.dnsValid(d), s.now())...)
		s.log.Printf("[api] DNS check %s: %s", d.Host, d.Status)
	}
}

func (s *Server) deleteDomain(r *http.Request, p params) result {
	_, d, err := s.resolve(r)
	if err != nil {
		return fail(err)
	}
	s.domains = filter(s.domains, func(x *Domain) bool { return x != d })
	msg := fmt.Sprintf("Custom domain %s (%s) deleted.", d.UUID, d.Host)
	return result{status: 200, body: map[string]string{"message": msg}, events: []event{d.webhookEvent("domain_deleted")}}
}

type verifyResponse struct {
	Message   string `json:"message"`
	DNSStatus string `json:"dns_status"`
	Host      string `json:"host"`
}

func (s *Server) verifyDomain(r *http.Request, p params) result {
	_, d, err := s.resolve(r)
	if err != nil {
		return fail(err)
	}
	events := d.applyDNS(s.dnsValid(d), s.now())
	body := verifyResponse{Message: "DNS records verification complete.", DNSStatus: dnsStatus(d.Status), Host: d.Host}
	return result{status: 200, body: body, events: events}
}

func (s *Server) purgeDomain(r *http.Request, p params) result {
	_, d, err := s.resolve(r)
	if err != nil {
		return fail(err)
	}
	msg := fmt.Sprintf("HTTP cache purge initiated for domain %s.", d.Host)
	return result{status: 202, body: map[string]string{"message": msg}}
}

// --- mock-only control endpoints ---

func (s *Server) mockSetDNS(r *http.Request, p params) result {
	host := normalizeHost(r.PathValue("host"))
	d := s.domainByHost(host)
	if d == nil {
		return fail(&apiError{Status: 404, Message: fmt.Sprintf("Custom domain %s not found.", host), Code: "not_found"})
	}
	if _, ok := p["valid"]; !ok {
		return fail(badRequest("valid_blank", `Send a JSON body like {"valid": true}`))
	}
	valid, err := p.boolean("valid", false)
	if err != nil {
		return fail(err)
	}
	d.dnsOverride = &valid
	events := d.applyDNS(valid, s.now())
	return result{status: 200, body: d.JSON(), events: events}
}

func (s *Server) mockReset(r *http.Request, p params) result {
	ups, doms, err := s.seedState()
	if err != nil {
		return fail(&apiError{Status: 500, Message: err.Error()})
	}
	s.upstreams, s.domains = ups, doms
	s.gen++ // pending DNS timers from before the reset become no-ops
	return result{status: 200, body: map[string]string{"message": "State reset."}}
}

func (s *Server) mockState(r *http.Request, p params) result {
	ups := make([]upstreamJSON, 0, len(s.upstreams))
	for _, u := range s.upstreams {
		ups = append(ups, u.JSON())
	}
	doms := make([]domainJSON, 0, len(s.domains))
	for _, d := range s.domains {
		doms = append(doms, d.JSON())
	}
	return result{status: 200, body: map[string]any{
		"account_uuid": s.cfg.AccountUUID, "upstreams": ups, "custom_domains": doms,
	}}
}

// --- request params ---

// params holds request attributes from a JSON body, a form body or the query
// string, mirroring how Rails merges them.
type params map[string]any

func readParams(r *http.Request) (params, *apiError) {
	p := params{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return nil, badRequest("bad_request", "Could not read request body.")
		}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &p); err != nil {
				return nil, badRequest("bad_request", "Request body is not a valid JSON object.")
			}
		}
	} else if r.Method != http.MethodGet {
		if err := r.ParseForm(); err != nil {
			return nil, badRequest("bad_request", "Request body could not be parsed.")
		}
		for k, v := range r.PostForm {
			p[k] = v[0]
		}
	}
	for k, v := range r.URL.Query() {
		if _, ok := p[k]; !ok {
			p[k] = v[0]
		}
	}
	return p, nil
}

func invalidParam(key string) *apiError {
	label := strings.ToUpper(key[:1]) + strings.ReplaceAll(key[1:], "_", " ")
	return badRequest(key+"_invalid", "%s is invalid", label)
}

func (p params) str(key string) (*string, *apiError) {
	switch v := p[key].(type) {
	case nil:
		return nil, nil
	case string:
		return &v, nil
	case float64, bool:
		s := fmt.Sprint(v)
		return &s, nil
	default:
		return nil, invalidParam(key)
	}
}

func (p params) strs(keys ...string) (map[string]*string, *apiError) {
	out := make(map[string]*string, len(keys))
	for _, k := range keys {
		v, err := p.str(k)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func (p params) boolean(key string, def bool) (bool, *apiError) {
	switch v := p[key].(type) {
	case nil:
		return def, nil
	case bool:
		return v, nil
	case string:
		if v == "" {
			return def, nil
		}
		if b, err := strconv.ParseBool(v); err == nil {
			return b, nil
		}
	}
	return false, invalidParam(key)
}

func (p params) integer(key string, def int) (int, *apiError) {
	switch v := p[key].(type) {
	case nil:
		return def, nil
	case float64:
		if v == float64(int(v)) {
			return int(v), nil
		}
	case string:
		if v == "" {
			return def, nil
		}
		if n, err := strconv.Atoi(v); err == nil {
			return n, nil
		}
	}
	return 0, invalidParam(key)
}

// --- helpers ---

type pagination struct {
	Page  int `json:"page"`
	Count int `json:"count"`
}

type listResponse[T any] struct {
	Data       []T        `json:"data"`
	Pagination pagination `json:"pagination"`
}

// paginate mirrors production: 20 per page by default, per_page capped at
// 100, pages past the end return the last page.
func paginate[S any, T any](items []S, q url.Values, render func(S) T) listResponse[T] {
	perPage, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || perPage < 1 {
		perPage = 20
	}
	perPage = min(perPage, 100)
	last := max(1, (len(items)+perPage-1)/perPage)
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	page = min(page, last)
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	data := make([]T, 0, end-start)
	for _, it := range items[start:end] {
		data = append(data, render(it))
	}
	return listResponse[T]{Data: data, Pagination: pagination{Page: page, Count: len(items)}}
}

func filter[T any](items []T, keep func(T) bool) []T {
	out := make([]T, 0, len(items))
	for _, it := range items {
		if keep(it) {
			out = append(out, it)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// statusWriter records the response status for logging. It forwards
// Flush, Hijack and Unwrap so streaming and upgrades keep working.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/_mock/health" {
			return // keep container healthchecks out of the log
		}
		target := r.URL.RequestURI()
		if prefix == "[proxy]" {
			target = r.Host + target
		}
		s.log.Printf("%s %s %s %d %s", prefix, r.Method, target, sw.status, time.Since(start).Round(100*time.Microsecond))
	})
}
