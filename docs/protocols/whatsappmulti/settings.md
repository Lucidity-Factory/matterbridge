# WhatsApp settings

> [!TIP]
> This page contains the details about whatsapp settings. More general information about whatsapp support in matterbridge can be found in [README.md](README.md).

## Number

Number you will use as a relay bot. Tip: Get some disposable sim card, don't rely on your own number.

- Setting: **REQUIRED**
- Format: *string*
- Example:
  ```toml
  Number="+48111222333"
  ```

## SessionFile

First time that you login you will need to scan QR code, then credentials will
be saved in a session file. If you do not set a `SessionFile`, you will need
to scan your QR code on every restart.

Unless this option is set, the Whatsapp login session is stored only in memory
until restarting matterbridge.

- Setting: **OPTIONAL**
- Format: *string*
- Example:
  ```toml
  SessionFile="/usr/share/matterbridge/session-48111222333.gob"
  ```

## PresenceMode

Controls the presence ("online" status) the bridge sends to WhatsApp after
each successful login, whenever the account's push name arrives or changes,
and on the interval set by `PresenceIntervalHours`. The WhatsApp library
refuses to send presence until the push name is known; on first pairing the
name arrives with the initial sync shortly after login, and the update is
sent then.

- `off`: send no presence. This is the behaviour of earlier versions.
- `unavailable`: send "unavailable" once per update. This gives WhatsApp the
  account's push name without marking the bridge as online.
- `blip`: send "available", wait `PresenceBlipSeconds`, then send
  "unavailable".
- `available`: send "available" and never "unavailable", so the bridge
  stays online for as long as it is connected. See the caveats below.

While the bridge is marked as available, delivery receipts for incoming
messages are sent as "active" rather than "inactive" (the bridge sends no
read receipts), and the primary phone has been reported not to receive push
notifications (whatsmeow issues #262 and #605).

In `blip` mode, keep the blip short. If sending "available" fails for any
reason other than a missing push name, "unavailable" is sent straight away.

`available` mode has these costs for as long as the bridge runs:

- The primary phone may get no push notifications for new messages. This is
  reported, not confirmed: in the issues above, one user found that
  "unavailable" restored notifications and another saw no difference.
- Contacts see the account as online while the bridge is connected, subject
  to the account's privacy settings.
- Delivery receipts are sent as "active" once the first update has been sent.
- WhatsApp only sends typing notifications to clients that are online, so
  the bridge may start receiving them. The bridge ignores them, but the
  WhatsApp library may log warnings about ones it cannot parse.

In `available` mode a failed send is not followed by "unavailable"; it is
retried on the next login, push name change or interval. Set
`PresenceIntervalHours` with this mode: without it, a send that fails while
the connection stays up is not retried until the next login. Nothing is sent
when the bridge stops.

- Setting: **OPTIONAL**
- Format: *string*
- Default: `off`
- Example:
  ```toml
  PresenceMode="blip"
  ```

## PresenceBlipSeconds

How long the bridge stays "available" in `blip` mode. Other modes ignore
it. A value of `0` or less uses the default.

- Setting: **OPTIONAL**
- Format: *int*
- Default: `5`
- Example:
  ```toml
  PresenceBlipSeconds=5
  ```

## PresenceIntervalHours

Repeat the `PresenceMode` update on this interval while connected and logged
in. `0` (or a negative value) only sends it after login and on push name
changes.

- Setting: **OPTIONAL**
- Format: *int*
- Default: `0`
- Example:
  ```toml
  PresenceIntervalHours=6
  ```
