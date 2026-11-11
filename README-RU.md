# srvimit

**Имитатор self-hosted сервисов.** Отдаёт байт-в-байт копию неавторизованной части
настоящего сервиса — страницу логина, все ассеты, которые грузит браузер, и реальные
ответы на попытку входа — на любом порту, прямо из записи на диске. Не нужны ни сам
сервис, ни база, ни сеть.

```bash
srvimit list                                   # что доступно
srvimit serve -service vaultwarden -port 8080  # имитировать сервис на порту
```

```
curl -i http://127.0.0.1:8080/                          → 200, настоящая оболочка веб-сейфа Vaultwarden
curl -i -X POST http://127.0.0.1:8080/identity/connect/token \
     -d 'grant_type=password&username=a&password=b'
                                                        → 400 {"message":"Username or password is incorrect",…}
```

[![ci](https://github.com/nrf24l01/proxy_fallback/actions/workflows/ci.yml/badge.svg)](https://github.com/nrf24l01/proxy_fallback/actions/workflows/ci.yml)
[![release](https://github.com/nrf24l01/proxy_fallback/actions/workflows/release.yml/badge.svg)](https://github.com/nrf24l01/proxy_fallback/actions/workflows/release.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

---

## Зачем

- **Демо и стенды**, которые выглядят как настоящий продукт, но без Grafana + Postgres
  + Redis за спиной.
- **Реалистичное HTTP-поведение для тестов и инструментов**: коды, заголовки, gzip и
  `chunked`, `ETag`/`304`, настоящие 404-страницы, настоящие отказы логина.
- **Воспроизвести состояние интерфейса** (экран логина, карточку ошибки) там, где
  исходный стек не запускается.
- **Офлайн-копия** лендинга сервиса для документации, ревью или архива.

Это не прокси и не мок: это **запись** реального инстанса, воспроизведённая с диска.

## Режимы

| Команда | Что делает |
|---|---|
| `list` | каталог: имя для `-service`, категория, путь логина, размер |
| `serve -service <имя> -port <порт>` | имитирует один сервис на одном порту |
| `serve -service a:8080 -service b:8081` | несколько сервисов в одном процессе |
| `serve -all -base-port 8000` | весь каталог подряд идущими портами |
| `record -config services.yaml` | обходит живые сервисы и пишет `fixtures/<имя>/` |
| `verify -config services.yaml` | сравнивает снапшот с живым апстримом |

Отдаётся байт-в-байт: статус, `Content-Type`, `Cache-Control`, `ETag`,
`X-Frame-Options`, `Content-Security-Policy`, `Vary`, `Content-Encoding` (gzip как
записан), `Location` и framing (`chunked` против `Content-Length` — как у апстрима).
Не отдаются только hop-by-hop заголовки, `Date` и `Set-Cookie`.

Детали, из-за которых это неотличимо от оригинала:

- **Неудачный вход 1:1** — реальные ответы записываются отдельно по типу контента:
  Grafana `401 {"message":"Invalid username or password"}`, Vaultwarden `400
  invalid_grant` на форму и `415` на JSON, git-сервер перерисовывает форму с ошибкой,
  Jenkins `302 → /loginError`. Любой пароль получает тот же отказ, что и у живого.
- **Неизвестные пути** отвечают как сам сервис (Grafana `302 → /login`, Gitea своим
  HTML-404, Portainer `404 page not found`).
- **Ничего не «заморожено»** — `Expires` пересчитывается из сохранённого TTL, CSP
  `nonce-…` генерируется на каждый ответ и переписывается в теле, `X-Request-Id`,
  `X-Trace-Id`, `X-RateLimit-*` создаются заново.
- **PROXY protocol v1** распознаётся и отрезается — листенер работает и за реверс-прокси
  или балансировщиком, которые его добавляют.
- Запись всегда идёт под **браузерным** User-Agent (сервисы от него зависят: Nextcloud
  отдаёт разный CSS браузеру и не-браузеру).

## Каталог

Снапшоты — это данные, а не код: они лежат в `fixtures/` и в git не попадают (см.
[Запись снапшотов](#запись-снапшотов)). Что можно записать, описано в
[`catalog/services.yaml`](catalog/services.yaml) — 35 сервисов, 3 отключены с указанием
причины.

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

Полный снапшот на 30 сервисов — ~175 МБ на диске. `srvimit list` показывает точные
цифры по тому, что записано у вас.

## Быстрый старт

```bash
go build -o bin/srvimit ./cmd/srvimit        # Go 1.24+, единственная зависимость — yaml.v3

./bin/srvimit list
./bin/srvimit serve -service grafana -port 8080 -listen 127.0.0.1

curl -sI http://127.0.0.1:8080/login
./tools/browser-smoke.sh http://127.0.0.1:8080/login fixtures/grafana "Welcome to Grafana"
```

### Запись снапшотов

Для записи нужен Docker: сервис поднимается по своему compose-определению, ждётся
готовности, снимается снапшот, контейнер гасится.

```bash
go build -o bin/catalogup ./cmd/catalogup
./bin/catalogup -services vaultwarden,grafana -capture     # два сервиса
./bin/catalogup -all -timeout 420s                         # весь каталог
./bin/catalogup -all -keep                                 # оставить контейнеры поднятыми
make catalog                                               # то же через make
```

`-capture` спрашивает headless Chromium, какие URL реально грузит страница, поэтому в
снапшот попадают ленивые JS-чанки и XHR-эндпоинты (без них SPA рисуется наполовину).

### Проверка снапшота

```bash
./bin/srvimit serve -service nextcloud -port 9502 &
./bin/srvimit verify -config verify.yaml                 # статус + заголовки + тело
./bin/srvimit verify -config verify.yaml -content-only   # только статус + тело
```

`verify` сравнивает с **живым** апстримом, поэтому записывайте и проверяйте на одном и
том же инстансе (у пересозданного Nextcloud свои ETag и ассеты темы — там нужен
`-content-only`). Референсный результат: `176/177 responses identical`, единственное
расхождение — `/backgrounds` у Nextcloud, который отдаёт случайную фотографию на каждый
запрос.

## Docker-образ

Образы публикуются в `ghcr.io/nrf24l01/proxy_fallback` на каждый тег (`latest` плюс сам
тег), сборка под `linux/amd64` и `linux/arm64`. Снапшоты — это данные, их монтируют:

```bash
docker run --rm -p 127.0.0.1:8080:8080 \
  -v "$PWD/fixtures:/data/fixtures:ro" \
  ghcr.io/nrf24l01/proxy_fallback serve -service grafana -port 8080 -listen 0.0.0.0
```

Собрать самому:

```bash
docker build -t srvimit .
docker run --rm -v "$PWD/fixtures:/data/fixtures:ro" srvimit list -fixtures /data/fixtures
```

## Docker Compose

Выбор сервиса происходит аргументом compose. Снапшоты уже внутри образа, монтировать
ничего не нужно:

```bash
docker compose up -d grafana            # имитировать ровно один сервис
docker compose up -d immich vaultwarden  # или несколько
docker compose up -d                    # все сервисы файла, по порту на каждый
docker compose logs -f grafana
docker compose down
```

`docker-compose.yml` **генерируется** из записанных снапшотов (один сервис на снапшот,
порты по порядку каталога с 8000):

```bash
srvimit compose > docker-compose.yml            # перегенерировать после пере-записи
srvimit compose -base-port 9000 -bind 0.0.0.0   # свои порты / адрес
```

На свежем клоне compose соберёт образ локально (секция `build:` входит в
сгенерированный файл); если уже скачан образ `ghcr.io/nrf24l01/proxy_fallback` —
используется он.

И образ, и контейнеры работают в read-only (`read_only: true`, `no-new-privileges`),
порты по умолчанию публикуются на `127.0.0.1`, а healthcheck проверяет листенер
`wget`-ом (рантайм Alpine, shell есть).


## Ограничения (честно)

- Это **снапшот**, а не прокси: существуют только записанные ответы. POST-логин, запись
  данных и WebSocket не работают — важен внешний вид, а не функциональность.
- Текст ошибки на неудачный вход зависит от тела запроса (Vaultwarden на неполную форму
  скажет `client_id cannot be blank`, снапшот — `Username or password is incorrect`);
  статус и структура совпадают, текст фиксированный.
- Эндпоинты, случайные по природе (`/backgrounds`), и значения конкретного инстанса
  (ассеты темы, ETag) воспроизвести нельзя — они стабильны, но привязаны к инстансу.
- Снапшоты стареют: после апгрейда сервиса нужен пере-`record`.
- Сервисы без страницы логина (Stirling PDF, ntfy, Prometheus) в каталог не берутся.

## Структура

```
cmd/srvimit/           CLI: list | serve | record | verify
cmd/catalogup/         поднимает сервисы каталога в Docker и записывает их
internal/config/       разбор services.yaml, дефолты, валидация
internal/recorder/     обход и запись (HTML/CSS/JS, обход браузером, POST-логины)
internal/fixture/      формат снапшота (manifest.json + bodies/)
internal/mirror/       отдача: заголовки, gzip, chunked, ETag/304, CSP nonce, 404
internal/proxyproto/   поддержка PROXY protocol v1
internal/catalog/      поиск снапшотов для -service
internal/verify/       сравнение с живым апстримом
catalog/services.yaml  что можно записать (образы, env, пути логина, POST-пробы)
tools/                 capture-urls.sh (Chromium), browser-smoke.sh
deploy/                systemd unit
docker-compose.yml     сгенерированный: один compose-сервис на снапшот (srvimit compose)
```

## CI/CD

| Workflow | Триггер | Что делает |
|---|---|---|
| [`ci.yml`](.github/workflows/ci.yml) | push в `main`/`master`, PR | `gofmt`, `go vet`, `go test -race`, сборка, тест определений каталога, multi-arch сборка образа (amd64 + arm64, без пуша) и smoke-тест образа |
| [`release.yml`](.github/workflows/release.yml) | тег `v*` (или вручную) | публикует multi-arch образ в `ghcr.io/nrf24l01/proxy_fallback` |

Опубликовать образ:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

## Лицензия

[MIT](LICENSE)
