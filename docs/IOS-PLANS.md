# iOS plan — heart-rate for walkpadspeed on iPhone

## Status

No iOS users today. This document records the architecture decision made
2026-11-16 so that iPhone support later is a matter of executing a plan,
not re-deciding it. Nothing iOS-specific is built yet; the only shared
piece (the walkpadspeed WebSocket heart-rate source) is deliberately
platform-neutral.

## Decision

**Chosen: the WebSocket variant** — a phone app that scans the strap as a
BLE central and pushes heart rate to walkpadspeed over a WebSocket, where
the same app serves the walkpadspeed web app locally.

**Not chosen: the GATT-server relay port** (a phone-side port of hr-relay
advertising itself as a standard HR belt for the phone's browser to
consume, like the Linux relay does today).

The reason is a hard fact about iPhones: they can never consume a
GATT-server relay from a browser, so that path would be Android-only work
that shares nothing with a future iOS path.

## Facts this rests on (verified 2026-11-16)

1. **No browser on iOS has Web Bluetooth.** All iOS browsers use WebKit
   (Safari, and Chrome/Firefox/Edge/Vivaldi there are WebKit re-shells),
   and WebKit has no Web Bluetooth API — caniuse shows "not supported"
   for Safari/iOS through iOS 27.x. So on an iPhone, walkpadspeed cannot
   reach the strap over BLE from *any* browser, regardless of what BLE
   devices exist around it. The Chrome 157 bug that motivates this relay
   is a Blink issue and does not exist on iOS — but neither does the
   working direct path.
2. **Safari blocks `ws://` from `https://` pages, including loopback.**
   WebKit deliberately rejects insecure WebSockets even to
   `127.0.0.1`/`localhost` (WebKit bug 171934), unlike Chrome which allows
   the localhost exception. So the naive shape — walkpadspeed served from
   GitHub Pages connecting to `ws://127.0.0.1` on the same phone — is
   blocked on iPhone. Chrome (Android/desktop) does allow
   `ws://localhost` from https pages; it may send a private-network
   preflight on the WS handshake, which the local server should answer.
3. **iOS randomizes BLE addresses**, so address matching (hr-relay's
   `-addr` path and its default) is useless on iOS. Scanning must match
   by advertised-name prefix — hr-relay's `-name` path is the right
   design to reuse.
4. **Apple distribution is the expensive half**: Apple Developer program
   ($99/yr), macOS for builds (GitHub Actions macOS runners work),
   TestFlight for distributing to the user. Debugging is remote-only
   without owning a device. CoreBluetooth central scanning itself is
   low-risk and well-trodden.

## Architecture chosen (identical on Android and iOS)

```
[ HR strap ] --BLE central--> phone app (local web+WS server)
                                  http://127.0.0.1:<port> serves walkpadspeed
                                  ws:// same origin: pushes bpm
                                        |
                                        v
                            walkpadspeed in the phone's browser
```

- **walkpadspeed gains a configurable WebSocket heart-rate source.** This
  is the one web change, shared by every platform (it also helps desktop
  Chromium users, who are hit by the same Chrome 157 bug, and enables
  server-side feeds — see next point).
- **The phone app serves walkpadspeed itself at `http://127.0.0.1:<port>`**
  and exposes the WS on the same origin. Same-origin WS means no mixed
  content in any browser, including Safari — this is what makes iPhone
  workable. The app bundles a copy of the walkpadspeed build (also gives
  offline use at the treadmill). No GATT server, no device chooser, and
  therefore none of the same-phone discovery uncertainties of a GATT
  relay (whether Android loops its own advertisements back to its own
  scanner is device-dependent and was never verified).
- **hr-relay on Linux gains a small WS endpoint** as a bonus transport:
  iPhone users at home get heart rate with zero Apple-side work —
  walkpadspeed connects to `wss://<home-domain>` (valid certificate via
  reverse proxy or Tailscale `serve`; valid-cert wss from an https page
  is fine in Safari, unlike ws).

## Build order

1. **walkpadspeed: configurable WS heart-rate source.** Testable today on
   Android/desktop with a fake-bpm WS server against the real https page.
   Required by every iPhone-capable scenario — do this first.
2. **hr-relay (Linux): WS endpoint.** Covers iPhone users *at home*
   immediately, with no phone app and no Apple account.
3. **Android app** (Kotlin): BLE central + local page/WS server. Fully
   testable here; serves untethered Android use. This replaces the
   GATT-relay port entirely.
4. **iOS app** — deferred until a user with an iPhone is ready to test
   (see next section). Same shape as the Android app; the BLE half is
   easy, the Apple distribution half is the work.

## When an iOS user shows up

Zero-code preflight, before writing anything:

- Have the user connect the COROS strap with nRF Connect on iOS. If it
  connects and shows live heart rate, the CoreBluetooth central half is
  proven for this strap, and the Chrome 157 class of bug is confirmed
  irrelevant on iOS.

Then the implementation path:

- Reuse the web change (already shipped by then) unchanged.
- Port the Android app's shape to Swift: CoreBluetooth central scanning
  by **name prefix** (never MAC), local HTTP+WS server on 127.0.0.1,
  walkpadspeed bundled.
- Distribution: Apple Developer account, CI build on a GitHub Actions
  macOS runner, TestFlight invite to the user. Remote debugging only.
- Fallbacks if anything blocks:
  - [iOSWebBLE](https://ioswebble.com), an App Store Safari extension
    that polyfills `navigator.bluetooth` over CoreBluetooth — lets an
    iPhone user try walkpadspeed's existing Web Bluetooth path directly
    (third-party, unverified by us).
  - The Linux-box wss feed (build step 2), which needs no phone app and
    covers the primary treadmill-at-home scenario.

## Why not the GATT-server relay port

A Kotlin port of hr-relay's GATT-server role (advertise 0x180D, serve
0x2A37, let the browser connect to the phone itself) is feasible on
Android but dead on arrival for iOS: no iOS browser can ever be its
client (fact 1). Choosing it would mean re-architecting again the day an
iPhone user appears. The WebSocket variant costs the same web change but
leaves iOS as "write the same app in Swift", not "invent a new path".
