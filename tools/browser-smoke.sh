#!/usr/bin/env bash
# Loads a mirrored page in headless Chromium and proves it renders like the real
# thing:
#   1. every URL the browser requested is present in the offline snapshot
#      (a missing asset would be a 404 -> broken page),
#   2. expected UI text is present in the rendered DOM.
#
#   ./tools/browser-smoke.sh <url> <fixture-dir|-> "<marker>" [<marker> ...]
set -uo pipefail

URL="${1:?url required}"
FIXTURE_DIR="${2:?- or fixture dir}"
shift 2
MARKERS=("$@")

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

chromium --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage \
  --no-first-run --no-default-browser-check --disable-extensions \
  --virtual-time-budget=12000 --window-size=1440,900 \
  --net-log-capture-mode=Everything --log-net-log="$TMPDIR/netlog.json" \
  --dump-dom "$URL" >"$TMPDIR/dom.html" 2>"$TMPDIR/chrome.log" || true

rc=0
echo "dom: $(wc -c <"$TMPDIR/dom.html")B"

for m in "${MARKERS[@]}"; do
  if grep -qiF -- "$m" "$TMPDIR/dom.html"; then
    echo "  ok    rendered: $m"
  else
    echo "  FAIL  not rendered: $m"
    rc=1
  fi
done

if [ "$FIXTURE_DIR" != "-" ]; then
  PAGE_URL="$URL" FIXTURE_DIR="$FIXTURE_DIR" python3 - "$TMPDIR/netlog.json" <<'PY' || rc=1
import json, os, re, sys
from urllib.parse import urlsplit

page = urlsplit(os.environ["PAGE_URL"])
raw = open(sys.argv[1], "rb").read().decode("utf-8", "replace")
requested = set()
for u in set(re.findall(r'"url"\s*:\s*"(https?://[^"]+)"', raw)):
    s = urlsplit(u)
    if s.netloc != page.netloc:
        continue
    p = s.path or "/"
    if s.query:
        p += "?" + s.query
    requested.add(p)

man = json.load(open(os.path.join(os.environ["FIXTURE_DIR"], "manifest.json")))
have = {e["key"] for e in man["entries"]} | {e["path"] for e in man["entries"]}
missing = sorted(p for p in requested if p not in have)
print(f"  browser requested {len(requested)} urls, {len(requested) - len(missing)} served from snapshot")
if missing:
    for p in missing:
        print("  FAIL  missing from snapshot:", p)
    sys.exit(1)
print("  ok    no unrecorded requests")
PY
fi

exit $rc