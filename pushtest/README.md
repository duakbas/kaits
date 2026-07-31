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

It **does not** remove the need for a phone you can install apps on. Permissions
come from the manifest, and a manifest only applies to an installed app — a
browser tab won't do. What it removes is the need to port the *entire* client to
find out.

## Running it

**1. Serve it.** The app must be reachable from the phone, so bind to your LAN,
not localhost. From the repo root:

```
python3 -m http.server 8000 --bind 0.0.0.0
```

**2. Start the catcher**, in another terminal:

```
python3 pushtest/catch.py serve
```

It prints your LAN IP. A push endpoint is ~200 characters; reading one off a
240x320 screen and retyping it is not a plan, so the phone posts it here.

**3. Point the app at the catcher.** Put that IP in `CATCHER` at the top of
`pushtest/index.html`.

**4. Install it on the phone.** With debugging enabled, WebIDE → *Install
hosted app* → the manifest URL:

```
http://<your-ip>:8000/pushtest/manifest.webapp
```

**5. Open it once.** It registers the worker, asks for notification permission,
subscribes, and posts the endpoint to the catcher. Every step prints pass or
fail on screen — if it fails, *that failure is the answer* and worth reporting
as-is.

**6. Now the actual test.** Close the phone. Wait — start with a minute, then
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
