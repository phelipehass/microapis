# microapis: Article Extractor & Markdown API

![CI](https://github.com/phelipehass/microapis/actions/workflows/ci.yml/badge.svg)

A small Go API that turns any web page into clean, LLM-ready Markdown. Built for RAG
pipelines and AI agents that need the main content of a page, not its menus, ads and
cookie banners.

The repository is a monorepo on purpose: future products get their own route prefix in the
same binary and share middleware, server and deploy.

## What it does

Give it a URL (or raw HTML) and it returns the main article content plus the metadata
you'd otherwise scrape by hand: title, author, publish date, language, cover image,
word count and reading time.

- **Tables survive.** Most converters flatten tables into unreadable text; this one keeps
  the columns, which is what matters most when feeding an LLM.
- **Absolute links.** Relative `href`s are resolved against the page URL.
- **Any encoding.** ISO-8859-1 pages come back as correct UTF-8.
- **Three formats:** `markdown`, `text` or clean `html`.
- **POST raw HTML** for pages behind a login or already fetched by the client.

## Quick start

```bash
go run ./cmd/server
curl "http://localhost:8080/v1/extract?url=https://go.dev/blog/go1.22"
curl "http://localhost:8080/v1/extract?url=https://example.com&format=text&include_images=false"
```

Without `RAPIDAPI_PROXY_SECRET`, gateway authentication is disabled (development mode).
In production the server refuses to start without it.

## Architecture

```
cmd/server          single binary, wiring and graceful shutdown
internal/config     configuration from environment variables
internal/safefetch  outbound HTTP with SSRF protection
internal/extract    readability + conversion (no network, tested in isolation)
internal/api        handlers and error → status code mapping
internal/httpx      middleware shared across products
openapi.yaml        API contract
```

The rule that keeps it healthy: `extract` knows nothing about the network, `safefetch`
knows nothing about inbound HTTP, and `api` only translates. That is what lets the whole
conversion be tested without starting a server.

## Security

An API that fetches arbitrary URLs is an SSRF target: without defenses, a client asks for
`http://169.254.169.254/` and reads your cloud instance credentials.

Protection has two layers:

1. **Before connecting:** URL validation rejects non-HTTP schemes and hosts that are
   literal internal IPs.
2. **At connect time:** a `Dialer.Control` hook validates the *resolved* IP. This closes
   DNS rebinding, where a domain resolves to a public IP during the check and to
   `127.0.0.1` on the next connection.

Blocked ranges include loopback, private, link-local (cloud metadata), CGNAT, NAT64 and
6to4 (both could smuggle an internal IPv4 address), plus documentation and benchmarking
blocks. Invalid IPs are blocked: the check fails closed.

On top of that:
- `Proxy: nil` on the transport (an `HTTP_PROXY` in the environment would otherwise make
  the check validate the proxy's IP instead of the target's)
- download size cap, redirect limit and timeouts on every stage
- the container runs as an unprivileged user

## Tests

```bash
go test ./...
```

No test makes real external calls: targets run on `httptest`. The most important test
proves that a URL pointing to the internal network returns `403`, so the API never
becomes a proxy into the host's network.

## Deploy

Requirements: Docker and Docker Compose on the server, and a domain pointing to it.

```bash
cp .env.example .env    # set DOMAIN and RAPIDAPI_PROXY_SECRET
docker compose up -d --build
curl https://your-domain.com/health
```

Caddy issues and renews the TLS certificate automatically. The API container is not
published to the internet (`expose`, no `ports`): the only way in is through Caddy.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Server port |
| `APP_ENV` | `development` | `production` enables JSON logs and requires the gateway secret |
| `RAPIDAPI_PROXY_SECRET` | — | Required in production |
| `FETCH_TIMEOUT` | `15s` | Max time to fetch the target page |
| `REQUEST_TIMEOUT` | `25s` | Max time for the whole request; keep above the previous one |
| `FETCH_MAX_BYTES` | `5242880` | Download cap (5 MiB) |
| `FETCH_MAX_REDIRECTS` | `5` | Redirect limit |
| `RATE_LIMIT_PER_MINUTE` | `120` | Per-subscriber infrastructure protection |
| `ALLOW_PRIVATE_IPS` | `false` | **Tests only.** Refused in production |

## License

MIT
