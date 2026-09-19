# Whatsapp

- Status: ???
- Maintainers: ???
- Features: ???

> [!INFO]
> 
> As of 2026, this is the new default whatsapp bridge implementation, based on the [whatsmeow](https://github.com/tulir/whatsmeow) library. This backend was previously known as `whatsappmulti`, but has been renamed `whatsapp`. You do not need to change your settings: they will still use the `whatsapp` config key.

## Configuration

> [!TIP]
> For detailed information about whatsapp settings, see [settings.md](settings.md)

**Basic configuration example:**

```toml
[whatsapp.mywhatsapp]
RemoteNickFormat="[{PROTOCOL}] @{NICK}: "
# Get a disposable SIM card
Number="+48111222333"
# See FAQ
SessionFile="/usr/share/matterbridge/session-48111222333.gob"
```

## FAQ

### What is the QR-code about?

The QR-Code is created by matterbridge on the terminal: It's the way WhatsApp authenticates any WA-Web session (which the bridge is using). At startup, matterbridge will prompt a QR-code for about 1 minute that you have to scan from your WA-instance (on your phone or android-VM). To scan it, go to Settings > WhatsApp Web in your WA client (if you run a VM, you may need to connect your webcam, or make a virtual one, see link above):

![](https://user-images.githubusercontent.com/8722846/64077252-ae3be180-ccce-11e9-812e-6e8a1e833346.png)

### How to set the WA-channel in the configuration file of matterbridge?

To setup a gateway between two protocols in matterbridge, you need to specify a channel for WA that should be bridged. The chat/group titles you see in WA won't work (e.g. from the screenshot above, "Test" won't work). Fortunately, matterbridge will complain about improper channel names and suggest the correct ones to you. In my case, it was a string of mainly numbers including the phone number of who created the group chat. Maybe there is a way to get this out of WA?
(Currently, the WA example config does not explain this at all; it doesn't even mention the gateway part.)

### Which channel formats are supported?

- `48111222333-1549986983@g.us`: a group JID. The bridge checks that the account is a member of the group and lists the joined groups if it is not.
- `48111222333@s.whatsapp.net`: a 1:1 chat with a contact, identified by phone number.
- `123456789012345@lid`: a 1:1 chat with a contact, identified by WhatsApp's Linked ID (LID). WhatsApp increasingly identifies contacts by LID instead of phone number.
- `status@broadcast`: accepted and ignored, so a configuration that lists it does not stop the bridge from starting.

1:1 chats are not checked against the joined groups. If the contact is not in the account's contact list, a warning is logged and the bridge continues.

Incoming direct messages addressed by LID are reported to the gateway under the contact's phone number JID whenever that number is known (from the message itself or from the session store), so one `@s.whatsapp.net` channel covers a contact in both forms. Without this, a contact configured as both an `inout` phone channel and an `in` LID channel would receive their own messages back through the phone channel. A `@lid` channel is only needed for a contact whose phone number is not known to the session.

### How to set a nice channel name?

Use `tengo`:

Save below snippet as `myremotenickformat.tengo`
```
if channel == "48111222333-1549986983@g.us" {
        result = "[CfP #Code for Pakistan] @"+nick
}
```
Add `RemoteNickFormat="{TENGO}"` to the specific bridge

and add the following in matterbridge.toml

```
[tengo]
RemoteNickFormat="myremotenickformat.tengo"
```

For context see: https://github.com/42wim/matterbridge/issues/725
