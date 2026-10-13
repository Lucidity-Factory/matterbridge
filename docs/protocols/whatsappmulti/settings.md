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

While the bridge is marked as available, delivery receipts for incoming
messages are sent as "active" rather than "inactive", and the primary phone
has been reported not to receive push notifications (whatsmeow issues #262
and #605), so keep the blip short. If sending "available" fails for any
reason other than a missing push name, "unavailable" is sent straight away.

- Setting: **OPTIONAL**
- Format: *string*
- Default: `off`
- Example:
  ```toml
  PresenceMode="blip"
  ```

## PresenceBlipSeconds

How long the bridge stays "available" in `blip` mode. A value of `0` or less
uses the default.

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
