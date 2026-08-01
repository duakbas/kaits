# Putting wad on a VPS

Running the daemon at home means the phone only works on your Wi-Fi. On a VPS it
works anywhere the phone has signal — which is the difference between a project
and a phone you actually carry.

Assumes Debian or Ubuntu and a domain you control. Everything here is one-time.

## The short version: one zip, one command

```bash
./wad/deploy/mkbundle.sh                       # on the laptop, daemon stopped
scp wad-setup-*.zip you@vps.example.com:
ssh you@vps.example.com
unzip wad-setup-*.zip && cd wad-setup && sudo ./install.sh wad.example.com
```

That is the whole deployment. [`install.sh`](deploy/install.sh) installs Go and
gcc, builds the daemon, creates the `wad` user and `/opt/wad`, puts your
**existing session** in place so there is no QR and no resync, installs and
starts the systemd service with a freshly generated token, installs Caddy and
gets a certificate for the hostname, and opens 80 and 443 while deliberately
leaving 8080 shut. It finishes by printing the two lines to type into the
phone's Settings screen. Re-running it is safe: every step checks first, and an
existing session or token is kept rather than overwritten.

[`mkbundle.sh`](deploy/mkbundle.sh) is what builds that zip. It refuses to run
while a local daemon is up, takes every `wa-session.db*` file including the
`-wal` sidecar, checksums them into the bundle and verifies the copy, and
vendors the Go dependencies so the server builds the versions that were
actually tested without needing to reach proxy.golang.org. About 6 MB plus your
message history. `--no-session` builds a code-only bundle if you would rather
pair with a QR on the server.

**The zip contains your WhatsApp account.** `wa-session.db` *is* the
linked-device registration, not a backup of it — anyone who unpacks the zip can
read and send your messages, and there is no password to change afterwards. Move
it with `scp`, not through anything that keeps a copy, and delete it from both
machines once the server is up. `mkbundle.sh` writes it mode `600` for the same
reason.

The rest of this document is what those two scripts are doing, and what to do
when the situation doesn't fit them.

## Which machine: check four things first

A rented VPS and a university box run the same steps. What differs is whether
they'll let you finish:

1. **Inbound connections from the internet.** The phone dials in, so something
   must accept 443 from outside. University networks very often firewall
   inbound, and that single fact decides everything below. Test from off-campus,
   not from a machine on their network.
2. **Root.** systemd units and binding 443 both need it. Without root you're
   running under `tmux` on a high port, which survives neither a reboot nor an
   admin.
3. **A hostname you can point at it.** Let's Encrypt needs a public name. A
   university machine may already have one, or you may not be able to add a
   record for it.
4. **Whether it's allowed.** A personal message relay running around the clock
   on university infrastructure is the kind of thing acceptable-use policies
   are written about. Worth knowing before rather than after — it's their
   hardware, and losing the account would be a bad way to find out.

If inbound is blocked or you don't have root — which is the common case on a
university box — **use a tunnel instead**, and none of the four matter:

```bash
# Cloudflare Tunnel: outbound-only, free, gives you a hostname with TLS.
cloudflared tunnel --url http://127.0.0.1:8080
```

That prints a public `https://…trycloudflare.com` address which proxies to your
local daemon, WebSockets included. Use `wss://<that host>` on the phone. The
throwaway URL changes on every restart; a named tunnel with your own domain is
the stable version and needs a free Cloudflare account. Tailscale is the other
option if the phone could join your tailnet, which on KaiOS it can't — so
Cloudflare is the practical one here.

A tunnel also removes the TLS section below entirely: the daemon stays on
loopback and never faces the internet at all.

## Two ways to bring the account across

**Re-pair on the server (simpler, and usually right).** Start the daemon there
with an empty directory, scan the QR, done. WhatsApp's initial sync delivers a
sizeable recent window across your chats — in this project's case roughly the
same 35k messages the local install had — so you are not losing much, and there
is nothing to copy.

What doesn't come back this way: stored media keys for old attachments, so
photos from before the re-pair need `WAD_REFETCH_MEDIA=1` to become viewable
again, and only as far back as your phone will re-send. Anything older than
WhatsApp's sync window is gone either way.

**Copy the session (keeps everything exactly).** Two files:

- `wa-session.db` — the session, the contacts, whatsmeow's own tables
- `wa-session.db.history.db` — every message we've stored

Copy both or you keep the account and lose the messages.

`wa-session.db` **is** the linked-device registration — copying it *moves* the
session rather than cloning it. Two daemons on the same file fight over one
WhatsApp device slot and at least one gets logged out, so **stop the local
daemon before starting the remote one** and don't leave a stray copy running.

Pairing survives the copy, so there's no QR to scan on this path.

## Doing it by hand

Everything below is the same deployment done step by step — useful when the box
isn't Debian, when you don't want a script touching the firewall, or when
something in `install.sh` failed and you need to know what it was in the middle
of.

## 1. Build

The daemon needs CGO for SQLite, so it can't be a bare `GOOS=linux go build`
from a Mac without a cross toolchain. Two options, and the second is less
trouble:

**Build on the VPS** (needs Go and gcc):

```bash
sudo apt update && sudo apt install -y golang gcc
git clone https://github.com/<you>/<repo>.git
cd <repo>/wad && go build -o wad ./cmd/wad
```

**Or cross-compile with zig**, if you'd rather not put a toolchain on the box:

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  CC="zig cc -target x86_64-linux-gnu" \
  go build -o wad ./cmd/wad
```

Check the VPS's architecture first — `uname -m`, usually `x86_64`, sometimes
`aarch64` on the cheap ARM instances.

## 2. Move the session

```bash
./wad/deploy/move-to-vps.sh you@vps.example.com
```

That does the whole step in the one order that is safe, and refuses rather than
half-doing it. What it is protecting against:

**Two daemons, one account.** `wa-session.db` *is* the linked-device
registration. A second daemon running against a copy is not a spare — WhatsApp
sees one device behaving like two, and the usual outcome is the session being
invalidated. The script refuses to run while a local daemon is up, and the
local one must stay down afterwards.

**WAL.** SQLite is in WAL mode here, so the database is not one file. A
`wa-session.db-wal` sidecar can hold committed transactions that are not yet in
the main file, and copying only `wa-session.db` from a daemon that did not exit
cleanly gives you a database that opens perfectly and is quietly missing your
most recent messages. Every `wa-session.db*` file goes across, and the script
says so when it finds a `-wal`.

**A truncated copy.** Both ends are checksummed and compared before anything is
started. `ls -l` agreeing is not the same as the bytes agreeing.

The history file can be large — 35k messages is tens of megabytes.

## 3. Run it as a service

Ready-made in [`deploy/wad.service`](deploy/wad.service) — copy it, then put the
token in a `systemctl edit` override rather than in the unit file, which is
world-readable. Anyone holding that token can talk to your WhatsApp account.

It binds the daemon to `127.0.0.1:8080` deliberately: TLS is terminated by the
proxy in front, and the phone must not be able to reach the daemon directly. A
token crossing the internet over plain `ws://` is a token given away.

For reference, the unit is:

```ini
[Unit]
Description=wad
After=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/wad
ExecStart=/opt/wad/wad
Environment=WAD_TOKEN=<a long random string>
Environment=WAD_ADDR=127.0.0.1:8080
Environment=WAD_DB=/opt/wad/wa-session.db
Restart=always
RestartSec=5
User=wad

[Install]
WantedBy=multi-user.target
```

`WAD_ADDR=127.0.0.1:8080` binds to loopback **on purpose**: the daemon has no
TLS of its own and a token is the only thing between the internet and your
WhatsApp account. The reverse proxy is what faces outward.

```bash
sudo useradd -r -s /usr/sbin/nologin wad
sudo chown -R wad:wad /opt/wad
sudo systemctl enable --now wad
sudo journalctl -u wad -f
```

Generate the token with something that isn't your cat's name:

```bash
openssl rand -hex 24
```

## 4. TLS, because the phone needs wss://

[`deploy/Caddyfile`](deploy/Caddyfile) is ready to use — edit the hostname and
reload. Caddy rather than nginx purely because it obtains the certificate on
its own, which is one less moving part on a box whose whole job is staying up.
Its proxy timeouts there are deliberately generous: the failure mode of a short
one is a 16 MiB attachment that silently never arrives.


Plain `ws://` sends your token in the clear on every connect, and KaiOS may
refuse a mixed-content socket outright. Caddy is the least work — it gets and
renews certificates by itself.

`/etc/caddy/Caddyfile`:

```
wad.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

```bash
sudo apt install -y caddy
sudo systemctl reload caddy
```

Caddy proxies WebSocket upgrades without extra configuration. Point the domain's
A record at the VPS **before** reloading, or the certificate request fails.

## 5. Point the phone at it

On the phone: chat list → bottom → **⚙ Settings**

```
Server address:  wss://wad.example.com
Token:           <the one from the service file>
```

No rebuild and no re-upload — that's why the address is entered on the phone
rather than baked into the package. `/ws` is appended automatically, and a typed
`wss://` keeps port 443 rather than having 8080 forced onto it.

## Afterwards

**Pairing survives the move.** The session file is the registration, so there's
no QR to re-scan. If WhatsApp does log the device out, re-pair on the VPS with
`journalctl -u wad -f` open — the QR renders in the log.

**Check the firewall.** Only 80 and 443 need to be open. If 8080 is reachable
from outside, the loopback bind above didn't take effect.

**Battery.** Away from home the phone is on mobile data with Wi-Fi off, which is
one radio instead of two — likely cheaper than the ~3%/hour measured at home
with Wi-Fi, LTE and Bluetooth all up.

**Back up the two db files** on a schedule. They are the account and every
message, and nothing else in this project is irreplaceable.
