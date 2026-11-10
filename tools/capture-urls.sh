#!/usr/bin/env bash
# Runs headless Chromium against a service and extracts every URL it requested,
# so the offline snapshot can be completed with lazy JS chunks / XHR endpoints.
#
#   ./tools/capture-urls.sh <page-url> <outfile> [allowed-host ...]
#
# Example:
#   ./tools/capture-urls.sh http://127.0.0.1:3000/ fixtures/grafana/urls.txt grafana.example.com
set -euo pipefail

PAGE_URL="${1:?page url required}"
OUTFILE="${2:?outfile required}"
shift 2
HOSTS=("$@")

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT
NETLOG="$TMPDIR/netlog.json"

CHROME_TIMEOUT="${CHROME_TIMEOUT:-60}"
TIMEOUT_BIN=$(command -v timeout || true)

# Some pages (Keycloak, heavy SPAs) never fire "load" in headless mode and would
# hang the whole pipeline; the netlog is written continuously, so a timeout still
# leaves us with usable data.
"${TIMEOUT_BIN:-echo}" "$CHROME_TIMEOUT" chromium --headless=new --no-sandbox \
  --disable-gpu --disable-dev-shm-usage \
  --no-first-run --no-default-browser-check --disable-extensions \
  --ignore-certificate-errors --hide-scrollbars --window-size=1440,900 \
  --virtual-time-budget=12000 \
  --net-log-capture-mode=Everything --log-net-log="$NETLOG" \
  --dump-dom "$PAGE_URL" >"$TMPDIR/dom.html" 2>"$TMPDIR/chrome.log" || {
    if [ -s "$NETLOG" ]; then
      echo "chromium hit the ${CHROME_TIMEOUT}s budget; using the netlog captured so far" >&2
    else
      echo "chromium failed, see log:" >&2
      tail -20 "$TMPDIR/chrome.log" >&2
      exit 1
    fi
  }

PAGE_URL="$PAGE_URL" OUTFILE="$OUTFILE" HOSTS="${HOSTS[*]:-}" python3 - "$NETLOG" <<'PY'
import json, os, re, sys
from urllib.parse import urlsplit

netlog = sys.argv[1]
page = urlsplit(os.environ["PAGE_URL"])
allowed = {h.strip() for h in os.environ.get("HOSTS", "").split() if h.strip()}
allowed.add(page.netloc)
outfile = os.environ["OUTFILE"]

raw = open(netlog, "rb").read().decode("utf-8", "replace")
urls = set(re.findall(r'"url"\s*:\s*"(https?://[^"]+)"', raw))

paths = set()
for u in urls:
    try:
        s = urlsplit(u)
    except ValueError:
        continue
    if s.netloc not in allowed:
        continue
    p = s.path or "/"
    if s.query:
        p += "?" + s.query
    paths.add(p)

if not paths:
    sys.exit("no URLs captured -- check --host-resolver-rules / allowed hosts")

os.makedirs(os.path.dirname(outfile) or ".", exist_ok=True)
with open(outfile, "w") as f:
    for p in sorted(paths):
        f.write(p + "\n")
print(f"{len(paths)} urls -> {outfile}")
PY