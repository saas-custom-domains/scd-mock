# scd-mock

A local mock of the [SaaS Custom Domains](https://saascustomdomains.com) API and proxy, for development and CI.

It runs two listeners in one small binary:

- **API** (port 4010): the upstream and custom domain endpoints from the [API docs](https://docs.saascustomdomains.com), with the same paths, JSON fields and status codes.
- **Proxy** (port 4080): forwards requests for your custom domains to your app, adding the same headers production adds (`X-Served-For`, `X-SaaS-Domains-IP`, `X-SaaS-Domains-Auth-Token`, ...).

It also simulates DNS checks and sends webhooks, so you can build and test a custom domains integration without an account, real DNS or TLS.

scd-mock keeps everything in memory. It is not connected to production and knows nothing about your real account.

## Quick start

With Docker:

```bash
docker run --rm -p 4010:4010 -p 4080:4080 \
  -e SCD_MOCK_UPSTREAM_HOST=host.docker.internal \
  -e SCD_MOCK_UPSTREAM_PORT=3000 \
  ghcr.io/saas-custom-domains/scd-mock:latest
```

On Linux, add `--add-host=host.docker.internal:host-gateway` so the container can reach your app on the host.

With Go (1.23 or newer):

```bash
go run github.com/saas-custom-domains/scd-mock@latest
# or, from a checkout
go run .
```

Out of the box you get:

| | |
|---|---|
| API base URL | `http://localhost:4010/api/v1` |
| API token | `scd_test_token` |
| Account UUID | `acc_local` |
| Upstream UUID | `upstream_local`, pointing at `localhost:3000` |
| Upstream auth token | `local_proxy_token` |
| Proxy | `http://<custom domain>:4080` |

Point your app's API base URL at the mock, then create custom domains on `upstream_local`. Hosts ending in `.localhost` pass the simulated DNS check after 2 seconds, and browsers and curl resolve `*.localhost` to `127.0.0.1`, so `http://acme.localhost:4080/` works with no setup.

See [docker-compose.example.yml](docker-compose.example.yml) for running the mock next to a sample app. That example publishes the proxy on port 80, so the URL is just `http://acme.localhost/`.

## Configuration

All settings are environment variables.

| Variable | Default | Description |
|---|---|---|
| `SCD_MOCK_API_ADDR` | `:4010` | API listen address. |
| `SCD_MOCK_PROXY_ADDR` | `:4080` | Proxy listen address. |
| `SCD_MOCK_API_TOKEN` | `scd_test_token` | Bearer token the API accepts. |
| `SCD_MOCK_ACCOUNT_UUID` | `acc_local` | The only account. Other account UUIDs return 404. |
| `SCD_MOCK_UPSTREAM_UUID` | `upstream_local` | UUID of the upstream created on startup. |
| `SCD_MOCK_UPSTREAM_HOST` | `localhost` | Host of the seeded upstream. In Docker use a service name or `host.docker.internal`. |
| `SCD_MOCK_UPSTREAM_PORT` | `3000` | Port of the seeded upstream. |
| `SCD_MOCK_UPSTREAM_TLS` | `false` | Use HTTPS to reach the seeded upstream. |
| `SCD_MOCK_UPSTREAM_AUTH_TOKEN` | `local_proxy_token` | Sent as `X-SaaS-Domains-Auth-Token` for the seeded upstream. |
| `SCD_MOCK_DOMAINS` | (empty) | Comma-separated hosts to create on the seeded upstream at startup, already valid. |
| `SCD_MOCK_DNS_DELAY` | `2s` | Time until a new domain's simulated DNS check runs. Go duration or seconds. |
| `SCD_MOCK_VALID_HOSTS` | (empty) | Comma-separated hosts that pass the DNS check, in addition to `*.localhost`. `*.example.com` matches any subdomain of `example.com` but not `example.com` itself. |
| `SCD_MOCK_WEBHOOK_URL` | (empty) | If set, webhooks are POSTed here. |
| `SCD_MOCK_WEBHOOK_TOKEN` | `local_webhook_token` | Added to the webhook URL as the `token` query param. |
| `SCD_MOCK_FORWARD_HOST` | `false` | Set `X-Forwarded-Host` to the custom domain instead of the upstream host. |
| `SCD_MOCK_KEEP_HOST` | `false` | Pass the client's `Host` header through to the upstream instead of replacing it. |
| `SCD_MOCK_FORWARDED_PROTO` | `https` | Value of `X-Forwarded-Proto`. Production always sends `https`. Set `http` when your app compares the browser's origin with its own scheme (CSRF checks in Rails, Django, SvelteKit) and you test over the mock's plain-HTTP port. |

## API

All `/api/v1` endpoints need `Authorization: Bearer <SCD_MOCK_API_TOKEN>`. Request bodies can be JSON or form-encoded (`curl -d host=...`), like production.

| Method | Path | Success |
|---|---|---|
| GET | `/api/v1/accounts/:account_uuid/upstreams` | 200, paginated, `?host=` filter |
| POST | `/api/v1/accounts/:account_uuid/upstreams` | 201 |
| GET | `/api/v1/accounts/:account_uuid/upstreams/:upstream_uuid` | 200 |
| DELETE | `/api/v1/accounts/:account_uuid/upstreams/:upstream_uuid` | 200, also deletes its custom domains |
| GET | `/api/v1/accounts/:account_uuid/upstreams/:upstream_uuid/custom_domains` | 200, paginated, `?host=` filter |
| POST | `/api/v1/accounts/:account_uuid/upstreams/:upstream_uuid/custom_domains` | 201 |
| GET | `.../custom_domains/:domain_uuid` | 200 |
| DELETE | `.../custom_domains/:domain_uuid` | 200 |
| POST | `.../custom_domains/:domain_uuid/verify_dns_records` | 200 |
| POST | `.../custom_domains/:domain_uuid/purge_http_cache` | 202 |

Errors:

| Case | Status | Body |
|---|---|---|
| Missing or wrong token | 401 | empty |
| Unknown account | 404 | `{"error":"Account acc_x not found."}` |
| Unknown upstream or domain | 404 | `{"error":"Custom domain domain_x not found.","error_code":"not_found"}` |
| Validation error | 400 | `{"error":"Host has already been taken","error_code":"host_taken"}` |
| Malformed JSON body | 400 | `{"error":"Request body is not a valid JSON object.","error_code":"bad_request"}` |

Validation error codes are `<attribute>_<problem>`, as in production: `host_blank`, `host_taken`, `host_invalid`, `port_invalid`, `tls_invalid`, `auth_token_invalid`, `prepend_path_invalid`, `bubble_target_path_invalid`, `challenge_type_invalid`, `instructions_recipient_invalid`, `redirect_to_www_invalid`, `meta_title_too_long`, `meta_description_too_long`.

Hosts are trimmed, lowercased and have a trailing dot removed before they are validated and stored. Custom domain hosts are unique across all upstreams. `prepend_path` is stored without leading or trailing slashes, like production.

Lists return `{"data": [...], "pagination": {"page": 1, "count": 42}}` with 20 items per page by default. `per_page` is capped at 100, and a `page` past the end returns the last page.

### DNS simulation

A new custom domain starts with `status: "pending_check"` and `tls_certificate_issued: false`. After `SCD_MOCK_DNS_DELAY`:

- hosts ending in `.localhost` or matching `SCD_MOCK_VALID_HOSTS` become `dns_records_found`, with `tls_certificate_issued: true` and `last_dns_check_at` set.
- all other hosts become `dns_records_not_found`.

`verify_dns_records` runs the same check right away and returns:

```json
{"message": "DNS records verification complete.", "dns_status": "valid", "host": "acme.localhost"}
```

`dns_status` is `valid` for `dns_records_found`, `pending` for `pending_check` and `invalid` for `dns_records_not_found`.

### Mock-only endpoints

These live on the API listener, need no token, and do not exist in production.

| Method | Path | Description |
|---|---|---|
| POST | `/_mock/domains/:host/dns` | Body `{"valid": true}` or `{"valid": false}`. Sets the DNS result for a domain now and for later checks. Returns the domain. |
| POST | `/_mock/reset` | Drops everything and recreates the seeded upstream and `SCD_MOCK_DOMAINS`. |
| GET | `/_mock/state` | Dumps all upstreams and custom domains. |
| GET | `/_mock/health` | Returns `{"status":"ok"}`. |

The Docker image's `HEALTHCHECK` runs `scd-mock healthcheck`, which calls `/_mock/health`.

## Proxy

The proxy takes the request's `Host` header, drops the port, and looks up a custom domain with that host.

- No such domain: `404` with the body `Unknown custom domain`.
- Domain exists but its status is not `dns_records_found`: `421 Misdirected Request` with a message saying what to do.
- Otherwise the request is forwarded to the domain's upstream (`http://host:port`, or `https://` if the upstream has `tls: true`).

Forwarded requests carry these headers. Any copies the client sent are removed first, so they cannot be spoofed through the proxy.

| Header | Value |
|---|---|
| `Host` | Upstream host, plus `:port` if the port is not 80 (HTTP) or 443 (HTTPS). The client's `Host` if `SCD_MOCK_KEEP_HOST=true`. |
| `X-Served-For` | The custom domain, without port. |
| `X-SaaS-Domains-IP` | Client IP. |
| `X-SaaS-Domains-Auth-Token` | The upstream's `auth_token`. |
| `X-Forwarded-For` | Client IP. |
| `X-Forwarded-Proto` | `https`, because production terminates TLS before your app. `SCD_MOCK_FORWARDED_PROTO=http` changes it for local testing. |
| `X-Forwarded-Host` | Same as `Host` by default. The custom domain if `SCD_MOCK_FORWARD_HOST=true`. |

If the domain has a `prepend_path`, it is added in front of the request path: with `prepend_path` `sites/client1`, `/about` is forwarded as `/sites/client1/about`.

Streaming responses and WebSocket upgrades are passed through.

## Webhooks

Set `SCD_MOCK_WEBHOOK_URL` to receive webhooks. Each one is a POST with the same JSON body as production to `<url>?token=<SCD_MOCK_WEBHOOK_TOKEN>` (if the URL already has a query string, `token` is added to it). It also carries the `X-SCD-Webhook-Token` header and `User-Agent: SaasCustomDomains.com`, like production.

Events sent:

| Event | When |
|---|---|
| `upstream_created`, `upstream_deleted` | Upstream created or deleted. |
| `domain_created`, `domain_deleted` | Custom domain created or deleted (also when its upstream is deleted). |
| `domain_dns_proxy_record_valid` | Status changes to `dns_records_found`. |
| `domain_dns_proxy_record_invalid` | Status changes to `dns_records_not_found`. |
| `tls_certificate_issued` | First time a domain becomes valid. |
| `domain_proxy_deployment_complete` | Right after the first valid check. |
| `domain_dns_challenge_record_valid`, `domain_dns_challenge_record_invalid` | `dns01` domains only, when the challenge status changes. |

Events are only sent on changes, so repeated checks with the same result send nothing. Deliveries run in the background, in order, with a 5 second timeout and up to 3 retries (1s, 2s, 4s backoff). API responses never wait for them. Every outcome is logged.

## Walkthrough

Start a tiny upstream that prints the headers it receives. Any app works; here is one in Python:

```bash
python3 - <<'EOF'
from http.server import BaseHTTPRequestHandler, HTTPServer
class Echo(BaseHTTPRequestHandler):
    def do_GET(self):
        body = f"{self.command} {self.path}\n{self.headers}".encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
HTTPServer(("127.0.0.1", 3000), Echo).serve_forever()
EOF
```

In another terminal, start the mock:

```bash
go run .
```

Create a custom domain on the seeded upstream:

```bash
curl -s http://localhost:4010/api/v1/accounts/acc_local/upstreams/upstream_local/custom_domains \
  -H "Authorization: Bearer scd_test_token" \
  -d host=acme.localhost
```

It starts as `pending_check`. Wait two seconds and check it:

```bash
curl -s -X POST http://localhost:4010/api/v1/accounts/acc_local/upstreams/upstream_local/custom_domains/<domain_uuid>/verify_dns_records \
  -H "Authorization: Bearer scd_test_token"
# {"message":"DNS records verification complete.","dns_status":"valid","host":"acme.localhost"}
```

Request it through the proxy:

```bash
curl -s http://acme.localhost:4080/hello
```

The upstream prints something like:

```
GET /hello
Host: localhost:3000
User-Agent: curl/8.7.1
Accept: */*
X-Forwarded-For: 127.0.0.1
X-Forwarded-Host: localhost:3000
X-Forwarded-Proto: https
X-Saas-Domains-Auth-Token: local_proxy_token
X-Saas-Domains-Ip: 127.0.0.1
X-Served-For: acme.localhost
Accept-Encoding: gzip
```

For hosts that do not resolve to your machine, send the `Host` header yourself or use `--resolve`:

```bash
curl -s -H "Host: shop.example.com" http://127.0.0.1:4080/
curl -s --resolve shop.example.com:4080:127.0.0.1 http://shop.example.com:4080/
```

(`shop.example.com` needs `SCD_MOCK_VALID_HOSTS=shop.example.com`, or a `POST /_mock/domains/shop.example.com/dns` with `{"valid": true}`, before the proxy forwards it.)

## Differences from production

scd-mock covers what most integrations use. It is not a full copy of production.

- **No TLS.** The proxy speaks plain HTTP. Production terminates TLS and issues certificates; the mock only flips `tls_certificate_issued`. `X-Forwarded-Proto` is still `https` by default so your app behaves as it would in production; set `SCD_MOCK_FORWARDED_PROTO=http` if a CSRF check compares schemes and you browse the mock over plain HTTP.
- **Simulated DNS.** No DNS lookups happen. A domain is valid if its host ends in `.localhost`, matches `SCD_MOCK_VALID_HOSTS`, or was set with `/_mock/domains/:host/dns`. Production checks for a CNAME to `in.saascustomdomains.com` (or A records) on a schedule; the mock checks once after `SCD_MOCK_DNS_DELAY` and on `verify_dns_records`.
- **Unknown and inactive domains.** The mock answers `404 Unknown custom domain` and `421` with a short explanation. In production these requests do not reach your app either, but the failure looks different (usually a TLS error, since no certificate is issued).
- **Status stays invalid.** Once a domain is `dns_records_not_found`, the proxy stops forwarding to it until it is valid again. `tls_certificate_issued` stays `true` once set, like production.
- **Host header port.** Production sends the upstream host without a port in `Host` and `X-Forwarded-Host`. The mock adds `:port` when the port is not the default for the scheme, because local apps usually listen on ports like 3000 and check the port.
- **Error texts.** Status codes and `error_code` values follow production. Some `error` messages, and the codes for mock-only checks such as `port_invalid` and `bad_request`, may differ.
- **One account.** Only `SCD_MOCK_ACCOUNT_UUID` exists. There is no subscription check (production returns 402 without an active subscription).
- **Upstream hosts.** Production requires a dotted public hostname and unique upstream hosts. The mock also accepts single-label hosts like `app` or `localhost`, and does not require upstream hosts to be unique, so several local upstreams can share `localhost` on different ports.
- **Wildcard custom domains** (`*.example.com`) are rejected with `host_invalid`.
- **Stored but not applied:** `redirect_to_www`, `bubble_target_path`, the `meta_*` fields, `compression_enabled`, `geocoding_enabled` and `bubble_io` are validated and returned, but the proxy ignores them. No instruction emails are sent, so `instructions_email_sent_at` is always `null`.
- **`purge_http_cache`** returns the production response and does nothing, since the mock has no cache.
- **Webhooks.** Production lets you register several webhooks per account and choose events; the mock sends every event to one URL. Retries are 3 with short backoff instead of 10 over several hours. In the `upstream_*` payloads, `port` is a number, which is what production sends (the docs example shows a string).
- **dns01 records.** `delegated_domain_control_validation_record_*` values for `dns01` domains are placeholders in the production format.
- **Other endpoints** (redirects, rules, analytics, custom certificates, `/api/v1/me`, updating resources) are not implemented.

## Development

```bash
go vet ./...
go test -race ./...
docker build -t ghcr.io/saas-custom-domains/scd-mock:latest .
```

Releases: pushing a `v*` tag builds `linux/amd64` and `linux/arm64` images and pushes them to `ghcr.io/saas-custom-domains/scd-mock`.

## License

MIT. See [LICENSE](LICENSE).
