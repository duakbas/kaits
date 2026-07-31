#!/usr/bin/env python3
"""catch.py — the computer half of the push test.

Two jobs:

  python3 catch.py serve     wait for the phone to post its push endpoint
  python3 catch.py ring      send an empty push to the saved endpoint

The endpoint is a capability URL: anyone holding it can wake your phone. It's
written to endpoint.txt next to this file, which is gitignored.
"""

import http.server
import os
import socket
import sys
import time
import urllib.request

PORT = 9999
HERE = os.path.dirname(os.path.abspath(__file__))
SAVED = os.path.join(HERE, "endpoint.txt")


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        endpoint = self.rfile.read(length).decode("utf-8", "replace").strip()
        with open(SAVED, "w") as f:
            f.write(endpoint)
        print("\ngot endpoint, saved to endpoint.txt:")
        print("  " + endpoint[:70] + ("…" if len(endpoint) > 70 else ""))
        print("\nNow: close the phone, wait, then run  python3 catch.py ring")
        self._cors(200)

    def do_OPTIONS(self):
        self._cors(200)

    def _cors(self, code):
        self.send_response(code)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Headers", "*")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *args):
        pass  # the prints above are the useful output


def lan_ip():
    """Best-effort LAN address — this is what goes in CATCHER in index.html."""
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect(("8.8.8.8", 80))  # no packets sent; just picks the route
        return s.getsockname()[0]
    except Exception:
        return "127.0.0.1"
    finally:
        s.close()


def serve():
    ip = lan_ip()
    print("listening on port %d" % PORT)
    print("set CATCHER in pushtest/index.html to:  http://%s:%d" % (ip, PORT))
    print("waiting for the phone…  (Ctrl-C to stop)")
    server = http.server.HTTPServer(("0.0.0.0", PORT), Handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nstopped")
    finally:
        server.server_close()


def ring():
    if not os.path.exists(SAVED):
        sys.exit("no endpoint.txt yet — run 'serve' and open the app on the phone first")
    with open(SAVED) as f:
        endpoint = f.read().strip()
    if not endpoint:
        sys.exit("endpoint.txt is empty")

    # An empty POST is all the Push API needs to wake a worker. TTL asks the
    # push service to hold it briefly rather than dropping it if the phone is
    # unreachable — which matters, since "asleep" is the case under test.
    req = urllib.request.Request(endpoint, data=b"", method="POST")
    req.add_header("TTL", "300")
    sent = time.strftime("%H:%M:%S")
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            print("%s  push accepted by the push service (HTTP %d)" % (sent, resp.status))
    except Exception as e:
        print("%s  push FAILED: %s" % (sent, e))
        print("A 404 or 410 means the subscription is dead — re-subscribe on the")
        print("phone (press 1 in the app) and run 'serve' again.")
        return
    print("\nAccepted only means the push service took it. Whether the PHONE woke")
    print("is the actual question — go look at it.")


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "serve"
    if cmd == "serve":
        serve()
    elif cmd == "ring":
        ring()
    else:
        sys.exit(__doc__)
