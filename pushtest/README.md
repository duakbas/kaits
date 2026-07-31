# Push test

Answers one question, which the whole project rests on and nobody has checked:

> **Does a Web Push wake a sideloaded KaiOS app while the phone is closed
> and asleep?**

Everything in `wad`'s push path assumes yes — the daemon stays connected to
WhatsApp, and when a message arrives it POSTs to the phone's endpoint to wake
the service worker. If the phone doesn't wake, that design doesn't work on this
hardware, and no amount of work on the app changes it.

This has nothing to do with WhatsApp. It's a manifest, a page, and a worker that
shows a notification. Install it, ring it, look at the phone.

## What it does and doesn't prove

It **does** answer, on real hardware:

- whether a push wakes the worker from standby, and after how long asleep;
- whether subscribing **without** an `applicationServerKey` is permitted (the
  real app relies on that — no VAPID means payload-free pushes, which is why
  message content never passes through Mozilla's or KaiOS's infrastructure);
- whether tapping the notification reopens the app;
- how long a subscription endpoint stays valid.

It **does not** work in a browser tab. Permissions come from the manifest, and a
manifest only applies to an installed app. What it removes is the need to port
the *entire* client to find out.

## Getting it onto the phone

Two routes. The first needs no debug access, which matters on a debug-locked
device.

### Route A — KaiStore test device (no debugging required)

The submission portal can distribute a build privately to specific handsets:
upload it, add your IMEI under **Test Device**, and it appears in KaiStore on
that phone alone. Nothing is published and no sideloading is involved, so a
locked bootloader and unauthorised ADB are irrelevant.

1. Set `CATCHER` in `index.html` first — it is baked into the package.
2. `./pushtest/package.sh` → `pushtest/build/pushtest.zip`
3. Upload the zip at the KaiOS submission portal.
4. **Test Device** → your IMEI (dial `*#06#` on the phone to get it).
5. Wait ~5 minutes, open KaiStore on the phone, install "Push Test".

### Route B — WebIDE (needs debugging enabled)

Serve the repo and install the hosted manifest directly:

```
python3 -m http.server 8000 --bind 0.0.0.0
# WebIDE -> Install hosted app -> http://<your-ip>:8000/pushtest/manifest.webapp
```

Faster to iterate on, and nothing leaves your machine — but only if the device
lets you in.

## Running the test

**1. Start the catcher**, on your computer:

```
python3 pushtest/catch.py serve
```

It prints your LAN IP. A push endpoint is ~200 characters; reading one off a
240x320 screen and retyping it is not a plan, so the phone posts it here. (If
the post fails, the endpoint is also printed on screen and can be transcribed —
tedious, but it unblocks you.)

**2. Open the app once.** It registers the worker, asks for notification
permission, subscribes, and posts the endpoint. Every step prints pass or fail
on screen — if it fails, *that failure is the answer* and worth reporting as-is.

**3. Now the actual test.** Close the phone. Wait — start with a minute, then
five, then an hour, then overnight. Then:

```
python3 pushtest/catch.py ring
```

Watch the phone.

## Reading the result

| What happens | What it means |
|---|---|
| Phone buzzes, notification appears | It works. The real app's design is sound on this device. |
| `ring` says accepted, phone silent | The push service took it but the phone didn't wake — the interesting failure. Try shorter sleeps to find where it stops. |
| `ring` gets 404 / 410 | The subscription expired. Re-subscribe (press `1` in the app) and note how long it lasted — the daemon needs to handle this. |
| Subscribe failed on the phone | Keyless subscribe isn't allowed here and the app needs a VAPID key after all. |

Worth recording the sleep duration each time. "Wakes after a minute but not
after an hour" is a completely different project from "never wakes", and both
are different from "always wakes".

## Keys on the phone

- `1` — re-subscribe (after a 404/410)
- `2` — local notification, no push involved. If this works but pushes don't,
  the problem is delivery, not notifications.

## endpoint.txt

Written by the catcher, gitignored. It's a capability URL: anyone holding it can
wake your phone. Don't paste it into a bug report.

## Finding, 31 July 2026 — KaiOS push is accepting and discarding

Tested on an Energizer E282SC (KaiOS 2.5), installed via the submission
portal's Test Device route — no debug access, no sideloading.

Everything on the app's side worked:

- installed from KaiStore on a debug-locked handset;
- service worker registered, notification permission granted;
- **keyless subscribe succeeded** — no `applicationServerKey` needed, which is
  what the payload-free design depends on;
- a local notification raised by the worker displays normally.

The push itself does not arrive. The endpoint is issued by
`notification.kaiostech.com:8443` — KaiOS's own infrastructure, not Mozilla's
autopush. `ring` gets HTTP 200 and the phone shows nothing, with the app open
and the worker alive. The page logs no `push` event at all.

The decisive detail: **after unsubscribing, the same endpoint still returns
HTTP 200.** A service tracking its subscriptions would answer 404 or 410. It
isn't checking, isn't queuing, and isn't delivering — it accepts everything and
discards it.

This also rules out the theory circulating in the BananaHackers Discord, that
the problem is clients mishandling *encrypted* pushes. This subscription is
keyless and the pushes carry no payload; there is nothing to decrypt.

### What it means

The blocker is upstream of anything in this repo. The daemon's push design is
sound and the phone is capable — a server at KaiOS is dropping the messages. If
that service is repaired, the existing code starts working with no changes.

### Re-testing

`python3 pushtest/catch.py ring` against a live endpoint. If the subscription
has expired, re-subscribe on the phone (`1`) with `catch.py serve` running.

### The open question this raises

If push stays dead, the fallback is for the app to hold its own connection and
raise notifications itself — a worker can call `showNotification()` for any
reason, not only from a push. That works only while the app is still running,
so the number that matters is **how long a KaiOS app survives after the flip is
closed**. The heartbeat answers it: open the app, close the phone, and watch
when the beats stop.

## Finding, 31 July 2026 — how long the app survives backgrounded

Same handset. The app beats every 15s and reports how long it has been alive,
so a restart is visible: `beat` and `aliveSeconds` both reset to zero.

**Idle: survives indefinitely.** 2h49m with the flip shut and no sign of
stopping. Background timers get throttled — 15s stretching to a plateau of
roughly 3 minutes — but the throttle is tied to the DEVICE being idle, not to
the app being backgrounded. Anything that wakes the phone snaps it straight back
to 15s while still reporting `hidden=True`.

**Survives the camera, and a phone call.** Both are large memory events and
neither evicted it.

**Killed by the browser.** After a stretch of YouTube the counters restarted
from `beat 1  alive 0s` — the low-memory killer took it. Nothing relaunches a
killed app, which is exactly the job Web Push would do if it worked.

### What this means for notifications

The WebSocket fallback works while the phone is idle or being used lightly, and
stops silently once something memory-hungry runs. Since the app can't be woken
again, the mitigation isn't background survival — it's making the app cheap to
come back to: on open, show what was missed while it was gone.

Timer throttling is also a floor rather than a ceiling for message latency. The
heartbeat is a `setInterval`, and timers are the first thing throttled; an
arriving message is a network event, which wakes the device. The real client
additionally gets a daemon ping every 20s, so it should stay far more responsive
than these numbers suggest.
