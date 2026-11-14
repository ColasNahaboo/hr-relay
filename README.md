# hr-relay

BLE heart-rate relay in Go: it connects to a standard BLE heart-rate sensor
(the COROS strap) and re-advertises itself as a standard heart-rate belt,
forwarding every measurement to the phone's browser over Web Bluetooth.

## Why

Chrome's Web Bluetooth path to the strap wedges: the link attaches instantly
to a stale connection and `startNotifications` never completes, while other
stacks — BlueZ on this machine, nRF Connect on the phone — talk to the same
strap fine. The relay moves the strap subscription to BlueZ here; the phone
only ever talks to this machine.

## Build

    go build -o hr-relay .

## Flags

| flag | default | meaning |
|---|---|---|
| `-addr` | `D1:F9:A6:A2:FC:6D` | sensor address; empty to match by name prefix |
| `-name` | `COROS HEART RATE` | sensor name prefix (used when `-addr` is empty) |
| `-advertise-name` | `HR RELAY` | name shown in the phone's device chooser |
| `-retry-secs` | `5` | seconds between sensor reconnect attempts |
| `-stale-secs` | `60` | drop the sensor link after this long without data (0 disables) |
| `-debug` | off | log every raw heart-rate notification |

## Service

Runs as a systemd user service (**disabled by default** — see "Coexistence"
below for why):

    systemctl --user start hr-relay     # enable and hold the strap
    systemctl --user disable --now hr-relay  # stand down again
    journalctl --user -u hr-relay -f
    systemctl --user restart hr-relay   # after code changes, or if advertising stalls

## Usage

In walkpadspeed on the phone, tap the ❤ HR box and pick **HR RELAY** in the
chooser. The real strap may not be listed while the relay holds its link —
that is expected. Everything else (zones, HR-driven speed control) works
unchanged, because the forwarded bytes are the strap's own.

The relay advertises continuously and reconnects to the strap automatically,
e.g. after you take it off and put it back on.

## Coexistence (2026-11-14)

The strap streams to exactly **one central at a time**: when the phone's
browser connects to the strap directly and subscribes, the strap stops
notifying the relay (and vice versa). With the relay running, its staleness
watchdog then disconnects after 60 s and re-subscribes every ~65 s, ripping
the notification stream away from the direct connection mid-workout. So:
run **either** the relay **or** a direct app-to-strap connection, never both.
That is why the service ships disabled; start it only when the direct path
is broken (the Chrome wedge).
