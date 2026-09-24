#!/usr/bin/env bash
#
# Build and deploy uptime-monitor: the Go daemon and the admin UI.
#
# Ordering is not arbitrary. The binary goes first because the UI calls
# endpoints (/api/sites/overview, /api/sites/{id}/series) that only exist in the
# newer binary — ship the UI first and the dashboard loads, then errors.
#
#   ./deploy/deploy.sh                 # build and deploy both
#   ./deploy/deploy.sh --binary-only   # skip the UI
#   ./deploy/deploy.sh --ui-only       # skip the daemon (no restart)
#   ./deploy/deploy.sh --host other    # override the ssh target
#
# Assumes an ssh alias (see docs/ROLLOUT.md §0) so no key path is needed here.

set -euo pipefail

HOST="${MONITOR_HOST:-ionos-vps}"
# Plesk owns this path; it is the document root of the monitor.pinkcrab.co.uk
# subscription. Do not relocate it — content outside Plesk's vhost tree fights
# its permission model.
DOCROOT="/var/www/vhosts/monitor.pinkcrab.co.uk/httpdocs"
PLESK_DOMAIN="monitor.pinkcrab.co.uk"
INSTALL_DIR="/opt/uptime-monitor"
SERVICE="uptime-monitor"
DO_BINARY=1
DO_UI=1

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary-only) DO_UI=0; shift ;;
    --ui-only)     DO_BINARY=0; shift ;;
    --host)        HOST="$2"; shift 2 ;;
    -h|--help)     sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

cd "$(dirname "$0")/.."
say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

say "Checking connectivity to $HOST"
ssh -o BatchMode=yes "$HOST" true

# ---------------------------------------------------------------- daemon ----
if [[ $DO_BINARY -eq 1 ]]; then
  say "Running tests"
  go vet ./...
  go test ./...

  say "Building linux/amd64 binary"
  make linux

  # A wrong-architecture binary starts fine locally and then fails on the server
  # with status=203/EXEC, which is an obscure way to find out. Check here.
  if ! file ./monitor | grep -q "x86-64"; then
    echo "ERROR: ./monitor is not linux/amd64:" >&2
    file ./monitor >&2
    echo "Run 'make linux' (amd64) or 'make linux-arm64' to match the server." >&2
    exit 1
  fi
  file ./monitor

  say "Backing up the current binary on $HOST"
  ssh "$HOST" "cp -f $INSTALL_DIR/monitor /root/monitor.previous 2>/dev/null || \
    echo 'no existing binary to back up'"

  say "Uploading binary"
  scp -q ./monitor "$HOST:/tmp/monitor.new"

  say "Installing and restarting $SERVICE"
  ssh "$HOST" "set -e
    mv /tmp/monitor.new $INSTALL_DIR/monitor
    chown uptime:uptime $INSTALL_DIR/monitor
    chmod 750 $INSTALL_DIR/monitor
    systemctl restart $SERVICE
    sleep 2
    systemctl is-active --quiet $SERVICE || { journalctl -u $SERVICE -n 30 --no-pager; exit 1; }"

  say "Health check (loopback, on the server)"
  ssh "$HOST" "curl -fsS http://127.0.0.1:8080/api/health && echo"
fi

# -------------------------------------------------------------------- UI ----
if [[ $DO_UI -eq 1 ]]; then
  say "Building admin UI"
  ( cd web && npm run build )

  if [[ ! -f web/dist/index.html ]]; then
    echo "ERROR: web/dist/index.html missing — the UI build produced nothing." >&2
    exit 1
  fi

  say "Syncing $DOCROOT"
  # --delete removes the previous build's content-hashed assets, which would
  # otherwise accumulate in the docroot indefinitely.
  #
  # --exclude protects Plesk's own files: it keeps per-domain artefacts (such as
  # .well-known challenges) that live in the document root but are not ours.
  rsync -az --delete --checksum \
    --exclude '.well-known' \
    web/dist/ "$HOST:$DOCROOT/"

  # Ownership under Plesk is suexec-based (the subscription's system user, group
  # psacln) and differs between files and directories. Rather than hard-coding a
  # user that changes if the subscription is recreated, let Plesk restore its own
  # model. Scoped to this one domain so no other subscription is touched.
  say "Restoring Plesk file permissions for $PLESK_DOMAIN"
  ssh "$HOST" "plesk repair fs $PLESK_DOMAIN -y" || {
    echo "WARNING: 'plesk repair fs' failed. The UI may return 403 until" >&2
    echo "permissions are corrected from the Plesk panel." >&2
  }
fi

say "Verifying the public endpoints"
# Through Cloudflare and Apache — what a browser and your consumers actually get.
BASE="https://monitor.pinkcrab.co.uk"
probe() { # path, expected-code, expected-content-type-fragment, note
  read -r code ctype < <(curl -s -o /dev/null --max-time 15 \
    -w '%{http_code} %{content_type}\n' "$BASE$1")
  local mark="ok "
  [[ "$code" == "$2" ]] || mark="FAIL"
  [[ -z "$3" || "$ctype" == *"$3"* ]] || mark="FAIL"
  printf '  %-4s %-28s %s %-24s %s\n' "$mark" "$1" "$code" "$ctype" "${4:-}"
}

probe /api/health            200 application/json
probe /                      200 text/html          "SPA shell"
probe /alerts                200 text/html          "client-side route"
probe /api/sites             401 application/json   "401 without credentials is correct"
# The fallback must not swallow API paths: a wrong path has to stay JSON.
probe /api/nope              404 application/json   "must NOT be text/html"
# Transitional. Once the legacy proxies are removed this becomes 200 text/html,
# which is the signal that any remaining consumer is now receiving the SPA shell
# instead of JSON — so check it before and after retiring them.
probe /health                200 application/json   "legacy path (transitional)"

say "Done"
