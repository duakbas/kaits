# Putting wad on a VPS

Running the daemon at home means the phone only works on your Wi-Fi. On a VPS it
works anywhere the phone has signal — which is the difference between a project
and a phone you actually carry.

Assumes Debian or Ubuntu and a domain you control. Everything here is one-time.

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

Stop the local daemon first. Then, from your machine:

```bash
scp wad/wa-session.db wad/wa-session.db.history.db you@vps:/opt/wad/
```

The history file can be large — 35k messages is tens of megabytes. Check it
arrived intact: `ls -l` on both ends should agree exactly.

## 3. Run it as a service

`/etc/systemd/system/wad.service`:

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
