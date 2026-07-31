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
