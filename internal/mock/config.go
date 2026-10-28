package mock

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config holds everything the mock needs. Use ConfigFromEnv to build it from
// SCD_MOCK_* environment variables.
type Config struct {
	APIAddr   string
	ProxyAddr string
	APIToken  string

	AccountUUID       string
	UpstreamUUID      string
	UpstreamHost      string
	UpstreamPort      int
	UpstreamTLS       bool
	UpstreamAuthToken string
	Domains           []string // pre-created as already valid

	DNSDelay   time.Duration
	ValidHosts []string // exact hosts or "*.example.com" suffix wildcards

	WebhookURL     string
	WebhookToken   string
	WebhookBackoff time.Duration // base retry backoff, not exposed as env

	ForwardHost bool // X-Forwarded-Host = custom domain instead of upstream host
	KeepHost    bool // pass the client's Host header through unchanged
}

// ConfigFromEnv reads the configuration using getenv (usually os.Getenv).
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	var errs []string
	boolean := func(key string, def bool) bool {
		v := str(key, "")
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %q is not a boolean", key, v))
		}
		return b
	}

	cfg := Config{
		APIAddr:           str("SCD_MOCK_API_ADDR", ":4010"),
		ProxyAddr:         str("SCD_MOCK_PROXY_ADDR", ":4080"),
		APIToken:          str("SCD_MOCK_API_TOKEN", "scd_test_token"),
		AccountUUID:       str("SCD_MOCK_ACCOUNT_UUID", "acc_local"),
		UpstreamUUID:      str("SCD_MOCK_UPSTREAM_UUID", "upstream_local"),
		UpstreamHost:      str("SCD_MOCK_UPSTREAM_HOST", "localhost"),
		UpstreamTLS:       boolean("SCD_MOCK_UPSTREAM_TLS", false),
		UpstreamAuthToken: str("SCD_MOCK_UPSTREAM_AUTH_TOKEN", "local_proxy_token"),
		Domains:           splitList(getenv("SCD_MOCK_DOMAINS")),
		ValidHosts:        splitList(getenv("SCD_MOCK_VALID_HOSTS")),
		WebhookURL:        str("SCD_MOCK_WEBHOOK_URL", ""),
		WebhookToken:      str("SCD_MOCK_WEBHOOK_TOKEN", "local_webhook_token"),
		WebhookBackoff:    time.Second,
		ForwardHost:       boolean("SCD_MOCK_FORWARD_HOST", false),
		KeepHost:          boolean("SCD_MOCK_KEEP_HOST", false),
	}

	port, err := strconv.Atoi(str("SCD_MOCK_UPSTREAM_PORT", "3000"))
	if err != nil || port < 1 || port > 65535 {
		errs = append(errs, "SCD_MOCK_UPSTREAM_PORT: must be a number between 1 and 65535")
	}
	cfg.UpstreamPort = port

	cfg.DNSDelay, err = parseDelay(str("SCD_MOCK_DNS_DELAY", "2s"))
	if err != nil {
		errs = append(errs, "SCD_MOCK_DNS_DELAY: "+err.Error())
	}

	if cfg.WebhookURL != "" {
		u, err := url.Parse(cfg.WebhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, "SCD_MOCK_WEBHOOK_URL: must be an absolute http(s) URL")
		}
	}

	if len(errs) > 0 {
		return cfg, fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}

// parseDelay accepts Go durations ("2s", "500ms") or a bare number of seconds.
func parseDelay(v string) (time.Duration, error) {
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		v = strconv.FormatFloat(n, 'f', -1, 64) + "s"
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a valid duration", v)
	}
	return d, nil
}

func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
