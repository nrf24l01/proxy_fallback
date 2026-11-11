# srvimit

**Imitate a self-hosted service.** Serve a byte-exact copy of a real service's
unauthenticated surface — its login page, every asset the browser loads and the real
answers it gives to a login attempt — on any port, straight from a recording on disk.
No service runtime, no database, no outbound network.

```bash
srvimit list                                   # what is available
srvimit serve -service vaultwarden -port 8080  # imitate one service on one port
```

```
curl -i http://127.0.0.1:8080/                          → 200, the real Vaultwarden web vault shell
curl -i -X POST http://127.0.0.1:8080/identity/connect/token \
     -d 'grant_type=password&username=a&password=b'
                                                        → 400 {"message":"Username or password is incorrect",…}
```

[![ci](https://github.com/nrf24l01/proxy_fallback/actions/workflows/ci.yml/badge.svg)](https://github.com/nrf24l01/proxy_fallback/actions/workflows/ci.yml)
[![release](https://github.com/nrf24l01/proxy_fallback/actions/workflows/release.yml/badge.svg)](https://github.com/nrf24l01/proxy_fallback/actions/workflows/release.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

---

## What it is good for

- **Demo and staging frontends** that look exactly like the real product, without
  running Grafana + Postgres + Redis behind them.
- **Realistic HTTP behaviour for tests and tooling**: status codes, headers, gzip and
  `chunked` framing, `ETag`/`304`, real 404 pages, real failed-login responses.
- **Reproducing a UI state** (a login screen, an error card) on a machine that cannot
  run the original stack.
- **Offline copies** of a service's landing page for docs, review or archival.

It is not a proxy and not a mock: it is a **recording** of a real instance, replayed
from disk.

## Modes

| Command | What it does |
|---|---|
| `list` | the catalog: service name, category, login path, size |
| `serve -service <name> -port <port>` | imitates one service on one port |
| `serve -service a:8080 -service b:8081` | several services in one process |
| `serve -all -base-port 8000` | the whole catalog on sequential ports |
| `record -config services.yaml` | crawls live services and writes `fixtures/<name>/` |
| `verify -config services.yaml` | compares the snapshot with the live upstream |

Replayed byte-for-byte: status, `Content-Type`, `Cache-Control`, `ETag`,
`X-Frame-Options`, `Content-Security-Policy`, `Vary`, `Content-Encoding` (gzip as
recorded), `Location` and the framing (`chunked` vs `Content-Length`, exactly like the
upstream). Only hop-by-hop headers, `Date` and `Set-Cookie` are not replayed.

Details that make it indistinguishable from the original:

- **Failed login is 1:1** — real responses to a wrong password are recorded per content
  type: Grafana `401 {"message":"Invalid username or password"}`, Vaultwarden `400
  invalid_grant` for form data and `415` for JSON, a git server re-renders its form with
  an error, Jenkins `302 → /loginError`. Any password gets the refusal the real service
  gives.
- **Unknown paths** answer with the service's own response (Grafana `302 → /login`,
  Gitea its HTML 404, Portainer `404 page not found`).
- **Nothing looks frozen** — `Expires` is recomputed from the recorded TTL, CSP
  `nonce-…` is minted per response and rewritten in the body, `X-Request-Id`,
  `X-Trace-Id` and `X-RateLimit-*` are regenerated.
- **PROXY protocol v1** is detected and stripped, so the listener also works behind a
  reverse proxy or load balancer that prepends it.
- Recording always speaks as a **browser** User-Agent (services branch on it: Nextcloud
  serves different CSS to browsers and to non-browsers).

## Catalog

Snapshots are data, not code: they live in `fixtures/` and are not committed (see
[Recording snapshots](#recording-snapshots)). What can be recorded is defined in
[`catalog/services.yaml`](catalog/services.yaml) — 35 services, 3 disabled with a
documented reason.

| Category | Service | `-service` | Login page | Responses | Size |
|---|---|---|---|---|---|
| ai | Open WebUI | `open-webui` | `/auth` | 111 | 4.6 MB |
| comms | Gotify | `gotify` | `/login` | 37 | 0.6 MB |
|  | Mattermost | `mattermost` | `/login` | 168 | 32.4 MB |
| data | Directus | `directus` | `/admin/login` | 59 | 11.5 MB |
|  | NocoDB | `nocodb` | `/signin` | 262 | 5.9 MB |
|  | PocketBase | `pocketbase` | `/_/` | 21 | 0.7 MB |
| dev | code-server | `code-server` | `/login` | 12 | 0.0 MB |
|  | Forgejo | `forgejo` | `/user/login` | 13 | 0.5 MB |
|  | Gitea | `gitea` | `/user/login` | 13 | 0.4 MB |
|  | Jenkins | `jenkins` | `/login` | 50 | 0.5 MB |
|  | Portainer | `portainer` | `/` | 17 | 2.5 MB |
| documents | BookStack | `bookstack` | `/login` | 15 | 0.4 MB |
|  | Paperless-ngx | `paperless` | `/accounts/login/` | 10 | 0.1 MB |
| files | File Browser | `filebrowser` | `/login` | 37 | 1.0 MB |
|  | Nextcloud | `nextcloud` | `/login` | 55 | 2.0 MB |
| finance | Actual Budget | `actual` | `/` | 54 | 12.5 MB |
| home | Mealie | `mealie` | `/login` | 154 | 0.7 MB |
| media | Audiobookshelf | `audiobookshelf` | `/login` | 25 | 2.7 MB |
|  | Navidrome | `navidrome` | `/app/` | 27 | 0.9 MB |
| monitoring | Grafana | `grafana` | `/login` | 39 | 9.7 MB |
|  | Uptime Kuma | `uptime-kuma` | `/` | 21 | 1.1 MB |
| network | AdGuard Home | `adguard` | `/login.html` | 13 | 0.7 MB |
|  | NetBird | `netbird` | `/` | 34 | 1.2 MB |
|  | Pi-hole | `pihole` | `/admin/login` | 52 | 2.0 MB |
| notes | Memos | `memos` | `/auth` | 54 | 2.0 MB |
| photos | Immich | `immich` | `/auth/login` | 95 | 5.2 MB |
|  | PhotoPrism | `photoprism` | `/library/login` | 46 | 10.3 MB |
|  | Photoview | `photoview` | `/login` | 18 | 0.3 MB |
| security | Keycloak | `keycloak` | `/admin/` | 81 | 7.1 MB |
|  | Vaultwarden | `vaultwarden` | `/` | 50 | 17.9 MB |

A full 30-service snapshot is ~175 MB on disk. `srvimit list` prints the exact numbers
for whatever you have recorded.

## Quick start

```bash
go build -o bin/srvimit ./cmd/srvimit        # Go 1.24+, one dependency (yaml.v3)

./bin/srvimit list
./bin/srvimit serve -service grafana -port 8080 -listen 127.0.0.1

curl -sI http://127.0.0.1:8080/login
./tools/browser-smoke.sh http://127.0.0.1:8080/login fixtures/grafana "Welcome to Grafana"
```

### Recording snapshots

Recording needs Docker: each service is started from its own compose definition, waits
until it answers, is recorded, and is torn down.

```bash
go build -o bin/catalogup ./cmd/catalogup
./bin/catalogup -services vaultwarden,grafana -capture     # two services
./bin/catalogup -all -timeout 420s                         # the whole catalog
./bin/catalogup -all -keep                                 # leave containers running
make catalog                                               # same, via make
```

`-capture` asks headless Chromium which URLs the page really loads, so lazy JS chunks
and XHR endpoints land in the snapshot too (a SPA without them renders half-broken).

### Verifying a snapshot

```bash
./bin/srvimit serve -service nextcloud -port 9502 &
./bin/srvimit verify -config verify.yaml                 # status + headers + body
./bin/srvimit verify -config verify.yaml -content-only   # status + body only
```

`verify` compares against the **live** upstream, so record and verify against the same
instance for a meaningful number (a recreated Nextcloud has its own ETags and theming
assets — use `-content-only` there). Reference result: `176/177 responses identical`,
the only difference being Nextcloud's `/backgrounds`, which serves a random photo per
request.

## Docker image

Images are published to `ghcr.io/nrf24l01/proxy_fallback` on every tag (`latest` plus the
tag itself), built for `linux/amd64` and `linux/arm64`. Snapshots are data — mount them:

```bash
docker run --rm -p 127.0.0.1:8080:8080 \
  -v "$PWD/fixtures:/data/fixtures:ro" \
  ghcr.io/nrf24l01/proxy_fallback serve -service grafana -port 8080 -listen 0.0.0.0
```

Build it yourself:

```bash
docker build -t srvimit .
docker run --rm -v "$PWD/fixtures:/data/fixtures:ro" srvimit list -fixtures /data/fixtures
```

## Docker Compose

Two files, both mounting snapshots read-only at `/data/fixtures`:

```bash
# one service (SERVICE / PORT from .env, defaults: grafana on 127.0.0.1:8080)
cp .env.example .env
docker compose up -d
curl -sI http://127.0.0.1:8080/login

# the whole catalog, sequential ports (8000, 8001, …)
docker compose -f docker-compose.catalog.yml up -d
curl -sI http://127.0.0.1:8000/login
./bin/srvimit list          # which service sits on which port

docker compose down         # or: -f docker-compose.catalog.yml down
```

Any setting is overridable without editing the files:

```bash
SERVICE=immich PORT=8081 docker compose up -d
IMAGE=ghcr.io/nrf24l01/proxy_fallback:latest FIXTURES_PATH=/srv/fixtures docker compose up -d
IMAGE=srvimit:local docker compose up -d      # use a locally built tag
```

Both files run the container read-only (`read_only: true`, `no-new-privileges`), publish
on `127.0.0.1` by default, and health-check through the binary itself (`srvimit list`),
since a distroless image has no shell or curl.

## Limitations (honest)

- A **snapshot**, not a proxy: only recorded responses exist. POST login, writes and
  WebSockets do not work — the point is the surface, not the functionality.
- The wording of a failed login depends on the request body (Vaultwarden says
  `client_id cannot be blank` for an incomplete form, the snapshot says
  `Username or password is incorrect`); status and shape match, the text is fixed.
- Endpoints that are random by nature (`/backgrounds`) or per-instance (theming assets,
  ETags) cannot be reproduced; they are stable but instance-specific.
- Snapshots age: re-record after upgrading a service.
- Services without a login page (Stirling PDF, ntfy, Prometheus) are deliberately not
  catalogued.

## Repository layout

```
cmd/srvimit/           CLI: list | serve | record | verify
cmd/catalogup/         starts catalog services in Docker and records them
internal/config/       services.yaml parsing, defaults, validation
internal/recorder/     crawl + record (HTML/CSS/JS, browser URL capture, POST logins)
internal/fixture/      snapshot format (manifest.json + bodies/)
internal/mirror/       replay: headers, gzip, chunked, ETag/304, CSP nonce, 404
internal/proxyproto/   PROXY protocol v1 support
internal/catalog/      snapshot lookup for -service
internal/verify/       upstream comparison
catalog/services.yaml  what can be recorded (images, env, login paths, POST login attempts)
tools/                 capture-urls.sh (Chromium), browser-smoke.sh
docker-compose.yml     run one service (SERVICE/PORT from .env)
docker-compose.catalog.yml  run the whole catalog on sequential ports
.env.example           defaults for both compose files
deploy/                systemd unit
```

## CI/CD

| Workflow | Trigger | What runs |
|---|---|---|
| [`ci.yml`](.github/workflows/ci.yml) | push to `main`/`master`, PR | `gofmt` check, `go vet`, `go test -race`, build, catalog definitions test, multi-arch docker build (amd64 + arm64, no push) plus an image smoke test |
| [`release.yml`](.github/workflows/release.yml) | tag `v*` (or manual) | publishes the multi-arch image to `ghcr.io/nrf24l01/proxy_fallback` |

Publish an image:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

## License

[MIT](LICENSE)