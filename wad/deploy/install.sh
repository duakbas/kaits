#!/usr/bin/env bash
# install.sh — set up wad on a fresh Debian/Ubuntu server, from nothing.
#
#   sudo ./install.sh wad.example.com
#
# Does the lot: toolchain, build, user, session files, service, firewall, TLS.
# Safe to re-run — every step checks before acting.

set -euo pipefail

HOST="${1:-}"
DIR=/opt/wad
BUNDLE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "ERROR: $*" >&2; exit 1; }
say() { echo; echo "==> $*"; }

[ "$(id -u)" -eq 0 ] || die "run with sudo"
[ -n "$HOST" ] || die "usage: sudo ./install.sh your.hostname"
case "$HOST" in *.*) ;; *) die "'$HOST' is not a hostname — TLS needs a real one";; esac

say "checking the hostname points here"
MYIP="$(curl -fsS https://api.ipify.org 2>/dev/null || true)"
HOSTIP="$(getent hosts "$HOST" | awk '{print $1}' | head -1 || true)"
if [ -n "$MYIP" ] && [ -n "$HOSTIP" ] && [ "$MYIP" != "$HOSTIP" ]; then
  echo "    WARNING: $HOST resolves to $HOSTIP but this box is $MYIP."
  echo "    Certificate issuance will fail until the DNS record points here."
  echo "    Continuing — fix the DNS and re-run, or Caddy will retry on its own."
elif [ -n "$MYIP" ]; then
  echo "    $HOST -> ${HOSTIP:-unresolved}, this box is $MYIP"
fi

say "installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl gcc ca-certificates ufw >/dev/null

# Debian's golang is too old for this module, so take it from upstream. The
# download list is authoritative for both the version and its checksum, so this
# verifies itself rather than trusting a hardcoded hash that rots.
if ! command -v /usr/local/go/bin/go >/dev/null 2>&1; then
  say "installing Go"
  ARCH="$(dpkg --print-architecture)"
  case "$ARCH" in
    amd64) GOARCH=amd64 ;;
    arm64) GOARCH=arm64 ;;
    *) die "unsupported architecture $ARCH" ;;
  esac
  # The listing is pretty-printed JSON and there is no jq on a fresh box, so
  # flatten it first and split it into one record per line on the braces. Each
  # record then holds both the filename and its hash, which is the whole point —
  # a hardcoded hash here would rot, and no hash at all would mean trusting the
  # transfer.
  META="$(curl -fsS 'https://go.dev/dl/?mode=json' | tr -d ' \t\n')"
  FILE="$(printf '%s' "$META" \
    | grep -o "\"filename\":\"go[0-9.]*\.linux-$GOARCH\.tar\.gz\"" | head -1 | cut -d'"' -f4)"
  [ -n "$FILE" ] || die "could not work out which Go to fetch"
  SHA="$(printf '%s' "$META" | tr '{}' '\n\n' | grep -F "\"filename\":\"$FILE\"" | head -1 \
    | grep -o '"sha256":"[0-9a-f]*"' | head -1 | cut -d'"' -f4)"
  [ ${#SHA} -eq 64 ] || die "could not read the checksum for $FILE"
  echo "    $FILE"
  curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/$FILE"
  echo "$SHA  /tmp/go.tgz" | sha256sum -c - >/dev/null || die "Go download failed its checksum"
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
fi
export PATH=/usr/local/go/bin:$PATH
echo "    $(go version)"

say "creating the wad user and $DIR"
id -u wad >/dev/null 2>&1 || useradd --system --home "$DIR" --shell /usr/sbin/nologin wad
mkdir -p "$DIR"

say "building"
rm -rf /tmp/wadsrc && mkdir -p /tmp/wadsrc
tar -C /tmp/wadsrc -xzf "$BUNDLE/src.tar.gz"
# The bundle normally carries vendor/, so this builds the exact dependency
# versions that were tested and needs no access to proxy.golang.org. Without it
# Go resolves the module graph itself, which needs outbound HTTPS.
if [ -d /tmp/wadsrc/vendor ]; then
  echo "    using the vendored dependencies"
  ( cd /tmp/wadsrc && go build -mod=vendor -o "$DIR/wad" ./cmd/wad )
else
  echo "    no vendor/ in the bundle — fetching modules"
  ( cd /tmp/wadsrc && go build -o "$DIR/wad" ./cmd/wad )
fi
echo "    $DIR/wad ($(du -h "$DIR/wad" | cut -f1))"

say "installing the session"
if ls "$BUNDLE"/session/wa-session.db* >/dev/null 2>&1; then
  if ls "$DIR"/wa-session.db* >/dev/null 2>&1; then
    echo "    a session is already here — keeping it, not overwriting."
    echo "    (delete $DIR/wa-session.db* first if you really mean to replace it)"
  else
    # Checked before it is put in place, not after: a truncated session database
    # opens perfectly well and then loses you the account.
    ( cd "$BUNDLE/session" && sha256sum -c SHA256SUMS --quiet ) \
      || die "the session files in this bundle are damaged — do not start the daemon, re-copy the zip"
    cp "$BUNDLE"/session/wa-session.db* "$DIR/"
    echo "    copied and verified; no re-pairing, no resync"
  fi
else
  echo "    no session in the bundle — this will print a QR to pair on first run"
fi
chown -R wad:wad "$DIR"
chmod 700 "$DIR"

say "service"
install -m 644 "$BUNDLE/wad.service" /etc/systemd/system/wad.service
mkdir -p /etc/systemd/system/wad.service.d
if [ ! -f /etc/systemd/system/wad.service.d/token.conf ]; then
  TOKEN="$(openssl rand -hex 16 2>/dev/null || head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  printf '[Service]\nEnvironment=WAD_TOKEN=%s\n' "$TOKEN" \
    > /etc/systemd/system/wad.service.d/token.conf
  chmod 600 /etc/systemd/system/wad.service.d/token.conf
  echo "    generated a new token"
else
  TOKEN="$(grep -o 'WAD_TOKEN=.*' /etc/systemd/system/wad.service.d/token.conf | cut -d= -f2)"
  echo "    keeping the existing token"
fi
systemctl daemon-reload
systemctl enable --now wad >/dev/null

say "TLS"
if ! command -v caddy >/dev/null 2>&1; then
  apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https >/dev/null
  curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    | tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
  apt-get update -qq && apt-get install -y -qq caddy >/dev/null
fi
sed "s/wad\.example\.com/$HOST/" "$BUNDLE/Caddyfile" > /etc/caddy/Caddyfile
systemctl enable caddy >/dev/null 2>&1 || true
systemctl restart caddy

say "firewall"
# 8080 is deliberately NOT opened: the daemon binds to localhost and everything
# reaches it through TLS. Opening it would make the token interceptable.
ufw allow OpenSSH >/dev/null 2>&1 || ufw allow 22/tcp >/dev/null
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw --force enable >/dev/null
ufw status | sed 's/^/    /'

say "done"
sleep 2
systemctl is-active --quiet wad && echo "    wad: running" || echo "    wad: NOT running — journalctl -u wad"
systemctl is-active --quiet caddy && echo "    caddy: running" || echo "    caddy: NOT running — journalctl -u caddy"

cat <<EOF

────────────────────────────────────────────────────────
On the phone, open Kaits → Settings and enter:

  Server address   wss://$HOST/ws
  Token            $TOKEN

────────────────────────────────────────────────────────
The first TLS certificate takes a few seconds. If the phone cannot connect
straight away, check: journalctl -u caddy -n 30

Do not start the old daemon on your laptop again. One account, one daemon.
EOF
