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
on the interval set by `PresenceIntervalHours`, and in `active` mode after
each message the bridge sends. The WhatsApp library
refuses to send presence until the push name is known; on first pairing the
name arrives with the initial sync shortly after login, and the update is
sent then.

- `off`: send no presence. This is the behaviour of earlier versions.
- `unavailable`: send "unavailable" once per update. This gives WhatsApp the
  account's push name without marking the bridge as online.
- `blip`: send "available", wait `PresenceBlipSeconds`, then send
  "unavailable". A login, push name change or interval tick during a blip
  sends "available" again; the blip still ends on schedule.
- `available`: send "available" and never "unavailable", so the bridge
  stays online for as long as it is connected. See the caveats below.
- `active`: go online when the bridge sends a message, and offline once
  `PresenceActiveMinutes` pass without one, the way a person's WhatsApp
  shows them online while they use it. See below.

While the bridge is marked as available, delivery receipts for incoming
messages are sent as "active" rather than "inactive" (the bridge sends no
read receipts). Being available does not silence the account's primary
phone: messages to the account still notify it. whatsmeow issue #262
reports the opposite.

In `blip` mode, keep the blip short. In `blip` and `active` modes, if
sending "available" fails for any reason other than a missing push name,
"unavailable" is sent straight away.

In `active` mode:

- Every message the bridge sends to WhatsApp successfully counts, whoever
  wrote it: relayed messages, bot messages, edits, deletes and files, and
  join, leave and topic messages when `ShowJoinPart`, `ShowTopicChange` or
  `SyncTopic` is set. A failed send does not count.
- The first send sends "available". Further sends only extend the time
  online; "unavailable" is sent `PresenceActiveMinutes` after the last one.
- A login or push name change sends "available" again while online, and
  "unavailable" otherwise.
- `PresenceIntervalHours` is ignored: a connection with no traffic stays
  offline. If the closing "unavailable" fails, it is sent again on the next
  login or push name change; a message starts a new online period, which
  ends with "unavailable" as usual.
- While online, the costs listed for `available` below apply.

`available` mode has these costs for as long as the bridge runs:

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
changes. Ignored in `active` mode.

- Setting: **OPTIONAL**
- Format: *int*
- Default: `0`
- Example:
  ```toml
  PresenceIntervalHours=6
  ```

## PresenceActiveMinutes

How long the bridge stays "available" in `active` mode after the last message
it sent. Other modes ignore it. A value of `0` or less uses the default.

- Setting: **OPTIONAL**
- Format: *int*
- Default: `5`
- Example:
  ```toml
  PresenceActiveMinutes=5
  ```
