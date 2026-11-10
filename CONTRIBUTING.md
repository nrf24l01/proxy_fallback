# Contributing

## Local checks (same as CI)

```bash
make check          # gofmt + go vet + go test -race + catalog validation
make build          # static binary in bin/
make cross          # linux/amd64, linux/arm64, darwin/arm64 into dist/
```

## Adding a service to the catalog

1. Add an entry to [`catalog/services.yaml`](catalog/services.yaml): image, host port,
   `title`, `category`, `login_path`, `wait` path, and — if the service has a JSON/form
   login endpoint — a `posts:` probe with deliberately wrong credentials.
2. Record it and check it renders:

   ```bash
   go run ./cmd/catalogup -services myservice -capture -keep
   ./bin/srvimit serve -service myservice -port 9501 &
   ./tools/browser-smoke.sh http://127.0.0.1:9501/login fixtures/myservice "Sign in"
   ```

3. Compare against the live container (`verify -config …`), then commit only the
   catalog change — snapshots stay out of git.

Rules that keep snapshots honest:

- Only services **with a login page** are catalogued.
- Record with the shared browser User-Agent (`config.DefaultUserAgent`); never with
  Go's default. Services branch on UA.
- If a service needs an admin to exist before the login page appears, use the `setup:`
  hooks rather than committing a wizard page.

## Changing the replay logic

Anything that alters what a probe sees belongs in `internal/mirror` with a unit test in
`mirror_test.go` (see `TestPostReplaysRecordedFailure`, `TestReplayPreservesHeadersAndBody`).
Header/behaviour changes must keep `verify` passing against a live upstream — say so in
the PR description, with the number from `verify` (`N/N responses identical`).

## Commit style

`<area>: <what changed>` — `mirror: regenerate CSP nonce per response`,
`catalogup: apply browser UA defaults`, `docs: add Nextcloud recording caveats`.