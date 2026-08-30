#!/usr/bin/env bash
# update-from-git.sh — update the daemon from a clone on the server itself.
#
#   ./wad/deploy/update-from-git.sh
#
# For when the laptop is not to hand. The bundle flow exists because mkbundle
# runs on the machine holding the session database; once the session is already
# on the server, nothing about an update needs a third computer. This pulls,
# vendors, applies the whatsmeow patch, builds, and restarts — everything the
# bundle would have carried, assembled in place.
#
# Needs: a clone the server can pull (a read-only deploy key is enough), Go,
# and sudo. install.sh has already put Go on the box.
#
# It does NOT touch the session, the token, Caddy or the firewall. Those are
# install.sh's business and none of them change between builds.

set -euo pipefail

WAD="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIR=/opt/wad

say() { echo; echo "==> $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || export PATH=/usr/local/go/bin:$PATH
command -v go >/dev/null 2>&1 || die "no go on PATH — run install.sh once first"

cd "$WAD"

say "pulling"
git -C "$WAD/.." pull --ff-only

say "vendoring"
# Vendored so the patch below has something local to apply to: the module cache
# is shared and read-only, and patching it would affect every build on the box.
go mod vendor
"$WAD/deploy/patch-vendor.sh" "$WAD"

say "building"
go build -mod=vendor -o /tmp/wad.new ./cmd/wad
echo "    $(du -h /tmp/wad.new | cut -f1)"

say "installing"
# Stop first. Replacing the binary under a running process is fine on Linux —
# the old inode stays alive — but restarting into a half-written file is not,
# and install(1) is not atomic across a full disk.
sudo systemctl stop wad
sudo install -m 755 -o root -g root /tmp/wad.new "$DIR/wad"
rm -f /tmp/wad.new
sudo systemctl start wad

sleep 2
if systemctl is-active --quiet wad; then
	echo "    wad: running"
else
	echo "    wad: NOT running — sudo journalctl -u wad -n 40" >&2
	exit 1
fi

say "built from"
git -C "$WAD/.." log --oneline -1
