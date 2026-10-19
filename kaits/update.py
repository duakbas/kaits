#!/usr/bin/env python3
"""Build Kaits from this checkout and update the copy on the phone in place.

  git pull && python3 kaits/update.py           # from the repo folder
  python3 kaits/update.py --version 0.0.20      # pick the version string yourself

USB plugged in, debug mode on (*#*#33284#*#*), phone unlocked.

Builds the certified package (calls on the earpiece, speaker, lid) and the
privileged one as a fallback with kaits/package.sh, then replaces the installed
sideloaded Kaits under the same app id — chats, settings and push stay. If
`ssh deniz` works, the daemon address + token are baked in as well, so even a
reinstall that wipes storage comes up logged in.
"""
import argparse, io, json, os, re, shutil, socket, subprocess, sys, tempfile, time, zipfile

PORT = 6000
HERE = os.path.dirname(os.path.abspath(__file__))
STATUS = {1: "web", 2: "privileged", 3: "certified"}


def die(msg):
    print("\n✗ " + msg)
    sys.exit(1)


def adb(*args):
    try:
        r = subprocess.run(["adb"] + list(args), capture_output=True, text=True, timeout=30)
    except FileNotFoundError:
        die("adb not found. Install it:  brew install android-platform-tools")
    if r.returncode != 0:
        die("adb %s failed: %s" % (" ".join(args), (r.stderr or r.stdout).strip()))
    return r.stdout


class RDP:
    def __init__(self):
        self.s = socket.create_connection(("127.0.0.1", PORT), timeout=20)
        self.s.settimeout(240)
        self.buf = b""
        self.read()

    def _more(self):
        try:
            chunk = self.s.recv(65536)
        except socket.timeout:
            die("The phone stopped answering. Unlock it, keep it awake, and run this again.")
        if not chunk:
            die("Phone closed the debugger connection.")
        self.buf += chunk

    def read(self):
        while b":" not in self.buf:
            self._more()
        head, _, self.buf = self.buf.partition(b":")
        n = int(head)
        while len(self.buf) < n:
            self._more()
        body, self.buf = self.buf[:n], self.buf[n:]
        return json.loads(body.decode("utf-8"))

    def request(self, packet, soft=False):
        data = json.dumps(packet).encode()
        self.s.sendall(str(len(data)).encode() + b":" + data)
        while True:
            reply = self.read()
            if reply.get("from") != packet["to"]:
                continue
            if "error" in reply:
                if soft:
                    return {"__error__": "%s: %s" % (reply["error"], reply.get("message", ""))}
                die("%s → %s: %s" % (packet.get("type"), reply["error"], reply.get("message", "")))
            if "type" in reply and reply["type"] != packet.get("type"):
                continue
            return reply


def build(version, kind):
    r = subprocess.run(["bash", os.path.join(HERE, "package.sh"), version, kind],
                       capture_output=True, text=True)
    if r.returncode != 0:
        die("building the %s package failed:\n%s%s" % (kind, r.stdout, r.stderr))
    return open(os.path.join(HERE, "build", "kaits.zip"), "rb").read()


def read_preset():
    """Daemon address + token from wad on deniz (optional)."""
    try:
        r = subprocess.run(["ssh", "-o", "ConnectTimeout=8", "deniz",
                            "sudo -n systemctl show wad -p Environment --value"],
                           capture_output=True, text=True, timeout=30)
        for part in r.stdout.split():
            if part.startswith("WAD_TOKEN="):
                return {"url": "wss://deniz.vsos.ethz.ch/ws", "token": part.split("=", 1)[1].strip('"')}
    except Exception:
        pass
    print("  (couldn't read the token from deniz — fine for an in-place update)")
    return None


def with_preset(pkg, preset):
    if not preset:
        return pkg
    zin = zipfile.ZipFile(io.BytesIO(pkg))
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zout:
        for info in zin.infolist():
            data = zin.read(info.filename)
            if info.filename == "js/config.js":
                data += ("\nwindow.KAITS_PRESET = %s;\n" % json.dumps(preset)).encode()
            zout.writestr(info, data)
    return out.getvalue()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--version", default=time.strftime("0.%y%m%d.%H%M"))
    a = ap.parse_args()

    print("• building Kaits %s from %s…" % (a.version, os.path.dirname(HERE)))
    pkgs = {k: build(a.version, k) for k in ("privileged", "certified")}
    shutil.rmtree(os.path.join(HERE, "build"), ignore_errors=True)
    print("✓ built (certified %d KB, privileged %d KB)" % (len(pkgs["certified"]) // 1024, len(pkgs["privileged"]) // 1024))

    if not [l for l in adb("devices").splitlines()[1:] if l.strip() and "device" in l]:
        die("No phone found. Dial *#*#33284#*#* on the phone and plug in USB.")
    adb("forward", "tcp:%d" % PORT, "localfilesystem:/data/local/debugger-socket")
    rdp = RDP()
    webapps = rdp.request({"to": "root", "type": "listTabs"}).get("webappsActor") or die("No app installer — debug mode on?")
    apps = rdp.request({"to": webapps, "type": "getAll"}).get("apps", [])
    mine = [x for x in apps if (x.get("name") or "").lower().startswith("kaits")
            or "kaits" in (x.get("manifestURL") or "").lower()]
    side = [x for x in mine if (x.get("manifestURL") or "").startswith("app://")]
    if side:
        app_id = re.match(r"^app://([^/]+)", side[0]["manifestURL"]).group(1)
        print("• updating the installed Kaits %s (id %s) in place" % ((side[0].get("manifest") or {}).get("version", ""), app_id))
    else:
        app_id = "kaits.local"
        print("• Kaits isn't sideloaded on this phone yet — installing it as %s" % app_id)

    preset = read_preset()

    def install(kind):
        pkg = with_preset(pkgs[kind], preset)
        up = rdp.request({"to": webapps, "type": "uploadPackage"})["actor"]
        for i in range(0, len(pkg), 20480):
            rdp.request({"to": up, "type": "chunk", "chunk": pkg[i:i + 20480].decode("latin-1")})
        rdp.request({"to": up, "type": "done"})
        print("• phone is unpacking (%s)…" % kind, flush=True)
        return rdp.request({"to": webapps, "type": "install", "appId": app_id, "upload": up}, soft=True)

    kind = "certified"
    res = install(kind)
    if res.get("__error__"):
        print("  phone refused the certified build (%s) — using the privileged one;" % res["__error__"])
        print("  calls then play from the loudspeaker.")
        kind = "privileged"
        res = install(kind)
    if res.get("__error__"):
        die("Phone refused the install: %s" % res["__error__"])

    url = "app://%s/manifest.webapp" % app_id
    rdp.request({"to": webapps, "type": "close", "manifestURL": url}, soft=True)
    time.sleep(1)
    rdp.request({"to": webapps, "type": "launch", "manifestURL": url}, soft=True)
    print("✓ Kaits %s installed (%s) and opened" % (a.version, kind))


if __name__ == "__main__":
    main()
