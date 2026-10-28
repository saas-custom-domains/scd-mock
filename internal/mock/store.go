package mock

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
)

// Custom domain statuses, as documented for the `status` field.
const (
	statusPendingCheck  = "pending_check"
	statusFound         = "dns_records_found"
	statusNotFound      = "dns_records_not_found"
	challengeHTTP01     = "http01"
	challengeDNS01      = "dns01"
	acmeValid           = "valid"
	acmePending         = "pending"
	acmeNotFound        = "not_found"
	apiTimeLayout       = "2006-01-02T15:04:05.000Z07:00" // Rails JSON timestamps
	webhookTimeLayout   = time.RFC3339                    // webhook timestamps, UTC
	defaultUpstreamPort = 443
)

type Upstream struct {
	UUID               string
	AccountUUID        string
	Host               string
	Port               int
	TLS                bool
	AuthToken          string
	BubbleIO           bool
	CompressionEnabled bool
	GeocodingEnabled   bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Domain struct {
	UUID                  string
	UpstreamUUID          string
	Host                  string
	PrependPath           *string
	BubbleTargetPath      *string
	MetaTitle             *string
	MetaDescription       *string
	MetaFaviconURL        *string
	MetaImageURL          *string
	ChallengeType         string
	RedirectToWWW         bool
	InstructionsRecipient *string
	Status                string
	TLSCertificateIssued  bool
	AcmeStatus            string
	LastDNSCheckAt        *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time

	// Not exposed by the REST API, but part of webhook payloads.
	ProxyDeploymentComplete    bool
	ProxyDeploymentCompletedAt *time.Time

	// Set by POST /_mock/domains/{host}/dns; overrides the host rules.
	dnsOverride *bool
}

// Live reports whether the proxy should forward traffic for this domain.
func (d *Domain) Live() bool { return d.Status == statusFound }

// upstreamJSON follows the production jbuilder view field order.
type upstreamJSON struct {
	UUID               string `json:"uuid"`
	Host               string `json:"host"`
	Port               int    `json:"port"`
	TLS                bool   `json:"tls"`
	CompressionEnabled bool   `json:"compression_enabled"`
	GeocodingEnabled   bool   `json:"geocoding_enabled"`
	BubbleIO           bool   `json:"bubble_io"`
	AuthToken          string `json:"auth_token"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	AccountUUID        string `json:"account_uuid"`
}

func (u *Upstream) JSON() upstreamJSON {
	return upstreamJSON{
		UUID: u.UUID, Host: u.Host, Port: u.Port, TLS: u.TLS,
		CompressionEnabled: u.CompressionEnabled, GeocodingEnabled: u.GeocodingEnabled,
		BubbleIO: u.BubbleIO, AuthToken: u.AuthToken,
		CreatedAt: apiTime(u.CreatedAt), UpdatedAt: apiTime(u.UpdatedAt),
		AccountUUID: u.AccountUUID,
	}
}

type domainJSON struct {
	UUID                     string  `json:"uuid"`
	Host                     string  `json:"host"`
	PrependPath              *string `json:"prepend_path"`
	BubbleTargetPath         *string `json:"bubble_target_path"`
	MetaTitle                *string `json:"meta_title"`
	MetaDescription          *string `json:"meta_description"`
	MetaFaviconURL           *string `json:"meta_favicon_url"`
	MetaImageURL             *string `json:"meta_image_url"`
	CreatedAt                string  `json:"created_at"`
	UpdatedAt                string  `json:"updated_at"`
	LastDNSCheckAt           *string `json:"last_dns_check_at"`
	Status                   string  `json:"status"`
	TLSCertificateIssued     bool    `json:"tls_certificate_issued"`
	AcmeStatus               string  `json:"acme_challenge_dns_record_status"`
	ChallengeType            string  `json:"challenge_type"`
	RedirectToWWW            bool    `json:"redirect_to_www"`
	InstructionsRecipient    *string `json:"instructions_recipient"`
	InstructionsEmailSentAt  *string `json:"instructions_email_sent_at"`
	UpstreamUUID             string  `json:"upstream_uuid"`
	DelegatedValidationHost  *string `json:"delegated_domain_control_validation_record_hostname"`
	DelegatedValidationValue *string `json:"delegated_domain_control_validation_record_value"`
}

func (d *Domain) JSON() domainJSON {
	j := domainJSON{
		UUID: d.UUID, Host: d.Host, PrependPath: d.PrependPath, BubbleTargetPath: d.BubbleTargetPath,
		MetaTitle: d.MetaTitle, MetaDescription: d.MetaDescription,
		MetaFaviconURL: d.MetaFaviconURL, MetaImageURL: d.MetaImageURL,
		CreatedAt: apiTime(d.CreatedAt), UpdatedAt: apiTime(d.UpdatedAt),
		LastDNSCheckAt: timePtr(d.LastDNSCheckAt, apiTimeLayout),
		Status:         d.Status, TLSCertificateIssued: d.TLSCertificateIssued,
		AcmeStatus: d.AcmeStatus, ChallengeType: d.ChallengeType, RedirectToWWW: d.RedirectToWWW,
		InstructionsRecipient: d.InstructionsRecipient, UpstreamUUID: d.UpstreamUUID,
	}
	if d.ChallengeType == challengeDNS01 {
		name := "_acme-challenge." + strings.TrimPrefix(d.Host, "*.")
		value := name + ".challenges.saascustomdomains.com"
		j.DelegatedValidationHost, j.DelegatedValidationValue = &name, &value
	}
	return j
}

// webhookPayload is a webhook body: the event name plus a domain or upstream.
type webhookPayload struct {
	Event    string               `json:"event"`
	Domain   *domainWebhookJSON   `json:"domain,omitempty"`
	Upstream *upstreamWebhookJSON `json:"upstream,omitempty"`
}

// domainWebhookJSON is the `domain` object in webhook payloads (webhooks.mdx).
type domainWebhookJSON struct {
	CreatedAt                  string  `json:"created_at"`
	UUID                       string  `json:"uuid"`
	UpstreamUUID               string  `json:"upstream_uuid"`
	Host                       string  `json:"host"`
	ChallengeType              string  `json:"challenge_type"`
	InstructionsRecipient      *string `json:"instructions_recipient"`
	LastDNSCheckAt             *string `json:"last_dns_check_at"`
	ProxyDeploymentComplete    bool    `json:"proxy_deployment_complete"`
	ProxyDeploymentCompletedAt *string `json:"proxy_deployment_completed_at"`
}

type upstreamWebhookJSON struct {
	CreatedAt string `json:"created_at"`
	UUID      string `json:"uuid"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	TLS       bool   `json:"tls"`
	BubbleIO  bool   `json:"bubble_io"`
}

func (d *Domain) webhookEvent(name string) event {
	return event{Name: name, Body: webhookPayload{Event: name, Domain: &domainWebhookJSON{
		CreatedAt: d.CreatedAt.UTC().Format(webhookTimeLayout), UUID: d.UUID,
		UpstreamUUID: d.UpstreamUUID, Host: d.Host, ChallengeType: d.ChallengeType,
		InstructionsRecipient:      d.InstructionsRecipient,
		LastDNSCheckAt:             timePtr(d.LastDNSCheckAt, webhookTimeLayout),
		ProxyDeploymentComplete:    d.ProxyDeploymentComplete,
		ProxyDeploymentCompletedAt: timePtr(d.ProxyDeploymentCompletedAt, webhookTimeLayout),
	}}}
}

func (u *Upstream) webhookEvent(name string) event {
	return event{Name: name, Body: webhookPayload{Event: name, Upstream: &upstreamWebhookJSON{
		CreatedAt: u.CreatedAt.UTC().Format(webhookTimeLayout), UUID: u.UUID,
		Host: u.Host, Port: u.Port, TLS: u.TLS, BubbleIO: u.BubbleIO,
	}}}
}

// applyDNS records a DNS check result and returns the webhook events caused by
// the change. Callers must hold the server lock.
func (d *Domain) applyDNS(valid bool, now time.Time) []event {
	var events []event
	d.LastDNSCheckAt, d.UpdatedAt = &now, now
	if valid {
		if d.Status != statusFound {
			d.Status = statusFound
			events = append(events, d.webhookEvent("domain_dns_proxy_record_valid"))
		}
		if d.ChallengeType == challengeDNS01 && d.AcmeStatus != acmeValid {
			d.AcmeStatus = acmeValid
			events = append(events, d.webhookEvent("domain_dns_challenge_record_valid"))
		}
		if !d.TLSCertificateIssued {
			d.TLSCertificateIssued = true
			events = append(events, d.webhookEvent("tls_certificate_issued"))
		}
		if !d.ProxyDeploymentComplete {
			d.ProxyDeploymentComplete, d.ProxyDeploymentCompletedAt = true, &now
			events = append(events, d.webhookEvent("domain_proxy_deployment_complete"))
		}
		return events
	}
	if d.Status != statusNotFound {
		d.Status = statusNotFound
		events = append(events, d.webhookEvent("domain_dns_proxy_record_invalid"))
	}
	if d.ChallengeType == challengeDNS01 && d.AcmeStatus != acmeNotFound {
		d.AcmeStatus = acmeNotFound
		events = append(events, d.webhookEvent("domain_dns_challenge_record_invalid"))
	}
	return events
}

// dnsStatus maps a domain status to the verify_dns_records `dns_status` value,
// the same way production does.
func dnsStatus(status string) string {
	switch status {
	case statusFound, "dns_check_unnecessary":
		return "valid"
	case statusPendingCheck:
		return "pending"
	default:
		return "invalid"
	}
}

// hostRuleValid decides whether a simulated DNS check passes for host.
func hostRuleValid(host string, validHosts []string) bool {
	if strings.HasSuffix(host, ".localhost") {
		return true
	}
	for _, pattern := range validHosts {
		pattern = strings.ToLower(strings.TrimSuffix(pattern, "."))
		if suffix, ok := strings.CutPrefix(pattern, "*"); ok {
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		} else if host == pattern {
			return true
		}
	}
	return false
}

// --- validation ---

// apiError is an error rendered as {"error": ..., "error_code": ...}.
type apiError struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
	Code    string `json:"error_code,omitempty"`
}

func (e *apiError) Error() string { return e.Message }

func badRequest(code, format string, args ...any) *apiError {
	return &apiError{Status: 400, Message: fmt.Sprintf(format, args...), Code: code}
}

var (
	labelRe       = `([a-z0-9]|[a-z0-9][a-z0-9-]*[a-z0-9])`
	hostnameRe    = regexp.MustCompile(`^(` + labelRe + `\.)*` + labelRe + `$`)
	authTokenRe   = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	pathRe        = regexp.MustCompile(`^[a-zA-Z0-9_:/-]*$`)
	maxMetaLength = 255
)

// normalizeHost lowercases, trims spaces and strips one trailing dot.
func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

func validHostname(h string) bool {
	if len(h) > 253 || !hostnameRe.MatchString(h) {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) > 63 {
			return false
		}
	}
	return true
}

func validateDomainHost(h string) *apiError {
	if strings.HasPrefix(h, "*.") {
		return badRequest("host_invalid", "Host %s is invalid. Wildcard custom domains are not supported by scd-mock.", h)
	}
	if !validHostname(h) || !strings.Contains(h, ".") {
		return badRequest("host_invalid", "Host %s is invalid. Please use something like page.domain.com, *.domain.com or domain.com", h)
	}
	return nil
}

// normalizePath strips leading/trailing slashes like production does and
// checks the allowed characters.
func normalizePath(attr, label string, p *string) (*string, *apiError) {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil, nil
	}
	v := strings.Trim(strings.TrimSpace(*p), "/")
	if !pathRe.MatchString(v) {
		return nil, badRequest(attr+"_invalid", "%s can only contain letters, numbers, dashes, underscores, colons, and forward slashes", label)
	}
	if v == "" {
		return nil, nil
	}
	return &v, nil
}

func validateEmail(p *string) *apiError {
	if p == nil {
		return nil
	}
	if a, err := mail.ParseAddress(*p); err != nil || a.Address != *p {
		return badRequest("instructions_recipient_invalid", "Instructions recipient is invalid")
	}
	return nil
}

func validateMeta(attr, label string, p *string) *apiError {
	if p != nil && len([]rune(*p)) > maxMetaLength {
		return badRequest(attr+"_too_long", "%s is too long (maximum is %d characters)", label, maxMetaLength)
	}
	return nil
}

// --- helpers ---

func newUUID(prefix string) string {
	return prefix + randomHex(4)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func apiTime(t time.Time) string { return t.UTC().Format(apiTimeLayout) }

func timePtr(t *time.Time, layout string) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(layout)
	return &s
}
