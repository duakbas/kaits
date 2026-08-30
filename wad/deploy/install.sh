#!/usr/bin/env bash
# install.sh — set up wad on a Debian/Ubuntu server, from nothing.
#
#   sudo ./install.sh wad.example.com
#
# Does the lot: toolchain, build, user, session files, service, firewall, TLS.
# Safe to re-run — every step checks before acting.
#
# On a box where 80 and 443 already belong to something else, put wad on its own
# port and leave the existing site alone:
#
#   sudo ./install.sh wad.example.com --port 8080 \
#        --cert /etc/letsencrypt/live/wad.example.com/fullchain.pem \
#        --key  /etc/letsencrypt/live/wad.example.com/privkey.pem
#
# Options:
#   --port N          serve on N instead of 443, leaving 80 and 443 untouched
#   --cert F --key F  use a certificate you already have rather than getting one
#   --no-tls          no TLS at all: plain ws://, token in the clear (see below)
#   --no-firewall     don't touch ufw — for a box whose firewall isn't yours
#   --replace-caddy   take over an existing Caddy config (kept as a .before-wad
#                     backup) instead of writing ours aside for you to import

set -euo pipefail

DIR=/opt/wad
BUNDLE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

HOST=""
PORT=443
CERT=""
KEYF=""
TLS=1
FIREWALL=1
REPLACE_CADDY=0

die() { echo "ERROR: $*" >&2; exit 1; }
say() { echo; echo "==> $*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --port) PORT="${2:-}"; [ -n "$PORT" ] || die "--port needs a number"; shift 2 ;;
    --cert) CERT="${2:-}"; [ -n "$CERT" ] || die "--cert needs a path"; shift 2 ;;
    --key)  KEYF="${2:-}"; [ -n "$KEYF" ] || die "--key needs a path"; shift 2 ;;
    --no-tls) TLS=0; shift ;;
    --no-firewall) FIREWALL=0; shift ;;
    --replace-caddy) REPLACE_CADDY=1; shift ;;
    -h|--help) sed -n '2,21p' "$0"; exit 0 ;;
    -*) die "unknown option: $1" ;;
    *) [ -z "$HOST" ] || die "more than one hostname given"; HOST="$1"; shift ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "run with sudo"
[ -n "$HOST" ] || die "usage: sudo ./install.sh your.hostname [--port N] [--cert F --key F]"
case "$HOST" in *.*) ;; *) die "'$HOST' is not a hostname — TLS needs a real one";; esac
case "$PORT" in ''|*[!0-9]*) die "--port must be a number";; esac
[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "--port out of range"

# Who owns port 80 decides whether a certificate can be obtained for a service
# that is on neither 80 nor 443: the ACME challenge is answered there. If the
# machine already runs Caddy and it holds :80, then adding a site on :8080 to
# THAT Caddy needs no certificate from you — it can get one itself, and the
# hostname is very likely one it already has a certificate for.
CADDY_ON_80=no
if command -v ss >/dev/null 2>&1; then
  ss -ltnp 2>/dev/null | awk '$4 ~ /:80$/' | grep -q caddy && CADDY_ON_80=yes
fi

# Otherwise the certificate has to come from somewhere: one that already exists
# on the box, or not at all.
if [ "$TLS" -eq 1 ] && [ "$PORT" -ne 443 ] && [ -z "$CERT" ] && [ "$CADDY_ON_80" != yes ]; then
  die "--port $PORT needs a certificate.

    Nothing can get one for you here: the ACME challenge is answered on 80 or
    443, which is exactly what you are working around. Two ways forward:

      --cert /path/fullchain.pem --key /path/privkey.pem
          Point at the certificate the existing site on 443 is already using.
          Find it with:  sudo caddy list-certificates 2>/dev/null;
                         ls /etc/letsencrypt/live/;
                         grep -rn ssl_certificate /etc/nginx/

      --no-tls
          Plain ws://$HOST:$PORT. Your token, which is the only thing between
          the internet and your WhatsApp account, then crosses the network in
          readable text on every single connect. Fine on a LAN. Not on a box
          with a public hostname."
fi
if [ "$TLS" -eq 1 ] && [ -n "$CERT" ]; then
  [ -n "$KEYF" ] || die "--cert given without --key"
  [ -r "$CERT" ] || die "cannot read $CERT"
  [ -r "$KEYF" ] || die "cannot read $KEYF"
fi
[ "$TLS" -eq 1 ] || CERT=""

# Where the daemon itself listens. Behind a proxy that is loopback; with
# --no-tls the daemon IS the public endpoint. And it cannot sit on the port the
# proxy in front of it is using.
if [ "$TLS" -eq 0 ]; then
  BIND="0.0.0.0:$PORT"
else
  INT=8080
  [ "$PORT" -ne "$INT" ] || INT=8081
  BIND="127.0.0.1:$INT"
fi

say "checking the hostname points here"
MYIP="$(curl -fsS https://api.ipify.org 2>/dev/null || true)"
# Public DNS, not the resolver. getent reads /etc/hosts first, and Ubuntu puts
# the machine's own hostname there pointing at 127.0.1.1 — so on exactly the
# box we are configuring, the check answered "127.0.1.1" and warned that the
# certificate would fail, every single run, while the DNS was perfectly fine.
HOSTIP=""
if command -v dig >/dev/null 2>&1; then
  HOSTIP="$(dig +short +time=3 +tries=1 A "$HOST" 2>/dev/null | grep -E '^[0-9.]+$' | head -1)"
elif command -v host >/dev/null 2>&1; then
  HOSTIP="$(host -t A "$HOST" 2>/dev/null | awk '/has address/ {print $4; exit}')"
fi
# No dns tools: fall back to the resolver, but ignore a loopback answer, which
# is the /etc/hosts entry rather than anything the world can see.
if [ -z "$HOSTIP" ]; then
  HOSTIP="$(getent hosts "$HOST" | awk '{print $1}' | grep -v '^127\.' | head -1 || true)"
fi
if [ -n "$MYIP" ] && [ -n "$HOSTIP" ] && [ "$MYIP" != "$HOSTIP" ]; then
  echo "    WARNING: $HOST resolves to $HOSTIP but this box is $MYIP."
  echo "    Certificate issuance will fail until the DNS record points here."
  echo "    Continuing — fix the DNS and re-run, or Caddy will retry on its own."
elif [ -n "$MYIP" ]; then
  echo "    $HOST -> ${HOSTIP:-unresolved}, this box is $MYIP"
fi

ARCH="$(dpkg --print-architecture)"

# A binary built elsewhere on a matching machine, if the bundle carries one.
# Worth checking before anything is installed: it removes the Go toolchain, the
# C compiler and the build cache from this box entirely, which between them are
# most of a gigabyte on a disk that may not have it.
PREBUILT="$BUNDLE/wad-linux-$ARCH"
[ -f "$PREBUILT" ] || PREBUILT=""

say "installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl ca-certificates ufw >/dev/null
# ffmpeg converts animated stickers to GIF. It is the only tool that reliably
# reads animated WebP, and without it those stickers fall back to a still frame
# — working, but not moving. Not fatal, so a failure here is not fatal either.
apt-get install -y -qq ffmpeg >/dev/null 2>&1 || \
  echo "    note: ffmpeg not installed; animated stickers will show as stills"
# gcc is only for building — the SQLite driver is CGO.
[ -n "$PREBUILT" ] || apt-get install -y -qq gcc >/dev/null

# Debian's golang is too old for this module, so take it from upstream. The
# download list is authoritative for both the version and its checksum, so this
# verifies itself rather than trusting a hardcoded hash that rots.
if [ -z "$PREBUILT" ] && ! command -v /usr/local/go/bin/go >/dev/null 2>&1; then
  say "installing Go"
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
if [ -z "$PREBUILT" ]; then
  export PATH=/usr/local/go/bin:$PATH
  echo "    $(go version)"
fi

say "creating the wad user and $DIR"
id -u wad >/dev/null 2>&1 || useradd --system --home "$DIR" --shell /usr/sbin/nologin wad
mkdir -p "$DIR"

if [ -n "$PREBUILT" ]; then
  say "installing the prebuilt binary"
  # A binary from a machine with a newer glibc will install perfectly and then
  # fail to start with a message only visible in the journal. Ask the loader
  # instead — it names the missing library, before anything depends on it.
  if ldd "$PREBUILT" 2>&1 | grep -q "not found"; then
    ldd "$PREBUILT" 2>&1 | grep "not found" | sed 's/^/    /'
    die "that binary needs libraries this machine does not have.
    It was built somewhere too new. Delete $(basename "$PREBUILT") from the
    bundle and re-run to build here instead."
  fi
  install -m 755 "$PREBUILT" "$DIR/wad"
  echo "    $DIR/wad ($(du -h "$DIR/wad" | cut -f1)) — no toolchain installed"
else
  say "building"
  rm -rf /tmp/wadsrc && mkdir -p /tmp/wadsrc
  tar -C /tmp/wadsrc -xzf "$BUNDLE/src.tar.gz"
  # The bundle normally carries vendor/, so this builds the exact dependency
  # versions that were tested and needs no access to proxy.golang.org. Without
  # it Go resolves the module graph itself, which needs outbound HTTPS.
  if [ -d /tmp/wadsrc/vendor ]; then
    echo "    using the vendored dependencies"
    ( cd /tmp/wadsrc && go build -mod=vendor -o "$DIR/wad" ./cmd/wad )
  else
    echo "    no vendor/ in the bundle — fetching modules"
    ( cd /tmp/wadsrc && go build -o "$DIR/wad" ./cmd/wad )
  fi
  echo "    $DIR/wad ($(du -h "$DIR/wad" | cut -f1))"
  rm -rf /tmp/wadsrc
fi

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
# The unit ships bound to 127.0.0.1:8080; a drop-in is applied after it, so this
# is what decides where the daemon actually listens.
printf '[Service]\nEnvironment=WAD_ADDR=%s\n' "$BIND" \
  > /etc/systemd/system/wad.service.d/addr.conf
echo "    listening on $BIND"
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
MANUAL_CADDY=""
if [ "$TLS" -eq 0 ]; then
  echo "    skipped (--no-tls). The token crosses the network in the clear."
else
  # Whether Caddy was serving something BEFORE we arrived decides whether its
  # config is ours to write. This has to be read before the install below, which
  # would start it.
  CADDY_WAS_UP=no
  systemctl is-active --quiet caddy 2>/dev/null && CADDY_WAS_UP=yes
  # Asked to take the box over: the config is ours to write, which also means
  # ours to be responsible for — including the global block, since we are now
  # the one running Caddy. The old config is kept as .before-wad below.
  if [ "$REPLACE_CADDY" -eq 1 ] && [ "$CADDY_WAS_UP" = yes ]; then
    echo "    taking over the existing Caddy config (--replace-caddy)"
    CADDY_WAS_UP=no
  fi

  if ! command -v caddy >/dev/null 2>&1; then
    apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https >/dev/null
    curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
      | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
      | tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
    apt-get update -qq && apt-get install -y -qq caddy >/dev/null
  fi

  # The site block.
  #
  # The global `auto_https disable_redirects` is there for one reason: a Caddy
  # WE start would otherwise also bind :80 to redirect to HTTPS, and on a box
  # where :80 belongs to another server that is either a failure to start or a
  # hijacked website. It must NOT be emitted when Caddy was already running,
  # because there can only be one global block and disabling redirects would
  # change the behaviour of the site already being served — which is the exact
  # thing this is trying not to do.
  SITE="$(mktemp)"
  if [ "$PORT" -eq 443 ] && [ -z "$CERT" ]; then
    sed "s/wad\.example\.com/$HOST/" "$BUNDLE/Caddyfile" > "$SITE"
  else
    {
      echo "# managed by wad install.sh"
      if [ "$CADDY_WAS_UP" != yes ]; then
        echo "{"
        echo "	auto_https disable_redirects"
        echo "}"
        echo
      fi
      echo "$HOST:$PORT {"
      [ -n "$CERT" ] && echo "	tls $CERT $KEYF"
      sed -n '/^wad\.example\.com {/,/^}/p' "$BUNDLE/Caddyfile" | sed '1d;$d'
      echo "}"
    } > "$SITE"
    # The template proxies to 127.0.0.1:8080, which is where the daemon is
    # UNLESS the public port is 8080 and it had to move out of the way.
    sed -i "s/127\.0\.0\.1:8080/127.0.0.1:$INT/" "$SITE"
    if [ -n "$CERT" ]; then
      # Caddy runs as its own user. A certificate it cannot read is a site that
      # does not come up, and the reason for that is buried in the journal.
      sudo -u caddy test -r "$CERT" 2>/dev/null && sudo -u caddy test -r "$KEYF" 2>/dev/null || {
        echo "    WARNING: the caddy user cannot read $KEYF."
        echo "    Grant it and restart, e.g.:"
        echo "        sudo setfacl -m u:caddy:r $KEYF"
        echo "        sudo setfacl -m u:caddy:x \$(dirname $KEYF)"
        echo "    (or add caddy to the group owning the key)"
      }
    fi
  fi

  # "Already ours" means the marker OR a config that already proxies to the
  # daemon — someone who wired Caddy up by hand has a working setup, and
  # telling them it is "NOT reachable yet" and to import a second site block
  # for the same hostname would break the thing that works.
  CADDY_IS_OURS=no
  if grep -q "managed by wad install.sh" /etc/caddy/Caddyfile 2>/dev/null; then
    CADDY_IS_OURS=yes
  elif grep -q "127\.0\.0\.1:$INT" /etc/caddy/Caddyfile 2>/dev/null; then
    CADDY_IS_OURS=yes
    echo "    Caddy already proxies to 127.0.0.1:$INT — leaving your config alone."
  fi

  if [ "$CADDY_WAS_UP" = yes ] && [ "$CADDY_IS_OURS" = no ]; then
    # Caddy was already serving somebody else's site. Overwriting its config
    # would take that site down, which is not this script's decision to make.
    install -m 644 "$SITE" /etc/caddy/wad.caddy
    MANUAL_CADDY=1
    echo "    Caddy is already running a config that isn't ours — NOT touching it."
    echo "    Wrote /etc/caddy/wad.caddy instead. Add this line to the top of"
    echo "    /etc/caddy/Caddyfile and run: sudo systemctl reload caddy"
    echo
    echo "        import /etc/caddy/wad.caddy"
    echo
    echo "    (if that file already has a global { } block at the top, move the"
    echo "     auto_https line into it instead of importing a second one)"
  else
    if [ "$CADDY_IS_OURS" = yes ] && ! grep -q "managed by wad install.sh" /etc/caddy/Caddyfile 2>/dev/null; then
      # Hand-wired and already pointing at us: nothing to do, and rewriting it
      # would throw away whatever else that file serves.
      echo "    nothing to change"
      rm -f "$SITE"
      SITE=""
    fi
    if [ -n "$SITE" ] && [ -f /etc/caddy/Caddyfile ] && ! grep -q "managed by wad install.sh" /etc/caddy/Caddyfile; then
      cp -n /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.before-wad" 2>/dev/null || true
      echo "    kept the previous config as /etc/caddy/Caddyfile.before-wad"
    fi
    if [ -n "$SITE" ]; then
      { echo "# managed by wad install.sh"; cat "$SITE"; } > /etc/caddy/Caddyfile
      systemctl enable caddy >/dev/null 2>&1 || true
      systemctl restart caddy
      echo "    serving $HOST on :$PORT"
    fi
  fi
  rm -f "$SITE"
fi

say "firewall"
if [ "$FIREWALL" -eq 0 ]; then
  echo "    skipped (--no-firewall) — make sure $PORT/tcp is reachable"
else
  # Only the public port is opened. When TLS terminates in front, the daemon's
  # own port stays shut deliberately: reaching it directly would mean handing
  # the token to the network in readable text.
  ufw allow OpenSSH >/dev/null 2>&1 || ufw allow 22/tcp >/dev/null
  ufw allow "$PORT"/tcp >/dev/null
  # 80 stays open for certificate renewal, and for whatever else is on this box.
  [ "$PORT" -eq 443 ] && ufw allow 80/tcp >/dev/null
  ufw --force enable >/dev/null
  ufw status | sed 's/^/    /'
fi

say "done"
sleep 2
systemctl is-active --quiet wad && echo "    wad: running" || echo "    wad: NOT running — journalctl -u wad"
if [ "$TLS" -eq 1 ]; then
  systemctl is-active --quiet caddy && echo "    caddy: running" || echo "    caddy: NOT running — journalctl -u caddy"
fi

# The address the phone needs, spelled out rather than described. Typing this on
# a T9 keypad is bad enough without having to work out the port.
if [ "$TLS" -eq 0 ]; then
  ADDR="ws://$HOST:$PORT/ws"
elif [ "$PORT" -eq 443 ]; then
  ADDR="wss://$HOST/ws"
else
  ADDR="wss://$HOST:$PORT/ws"
fi

cat <<EOF

────────────────────────────────────────────────────────
On the phone, open Kaits → Settings and enter:

  Server address   $ADDR
  Token            $TOKEN

────────────────────────────────────────────────────────
EOF

[ -n "$MANUAL_CADDY" ] && echo "NOT reachable yet — import /etc/caddy/wad.caddy first, as above."
if [ "$TLS" -eq 1 ] && [ "$PORT" -eq 443 ] && [ -z "$CERT" ]; then
  echo "The first certificate takes a few seconds. If the phone cannot connect"
  echo "straight away: journalctl -u caddy -n 30"
fi
if [ "$TLS" -eq 0 ]; then
  cat <<EOF
This is plain ws://. The token above is the only thing standing between the
internet and your WhatsApp account, and it now travels in readable text on
every connect. Put a certificate in front of it when you can:
  sudo ./install.sh $HOST --port $PORT --cert /path/fullchain.pem --key /path/privkey.pem
EOF
fi

echo
echo "Do not start the old daemon on your laptop again. One account, one daemon."
