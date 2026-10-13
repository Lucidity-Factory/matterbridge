package bwhatsapp

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	// qrDeadline bounds the whole QR pairing wait. whatsmeow itself gives up
	// after about 160s (six codes) and closes the channel; this only guards
	// against the channel never closing.
	qrDeadline = 5 * time.Minute

	// loginTimeout bounds the wait for whatsmeow's Connected event. On first
	// pairing it follows the 515 reconnect and the prekey upload.
	loginTimeout = 60 * time.Second
)

var (
	errQRFailed     = errors.New("WhatsApp QR pairing failed")
	errLoginFailed  = errors.New("WhatsApp login failed")
	errLoginTimeout = errors.New("timed out waiting for WhatsApp login")
)

// waitForQR shows each QR code and waits for the channel to close. The
// result is decided by the last event before the close: whatsmeow sends a
// final "success", "timeout" or error event and then closes the channel, and
// some events before that (such as "err-scanned-without-multidevice") are not
// final.
func waitForQR(qrChan <-chan whatsmeow.QRChannelItem, deadline time.Duration, onEvent func(whatsmeow.QRChannelItem)) error {
	var last whatsmeow.QRChannelItem

	timer := time.NewTimer(deadline)
	defer timer.Stop()

	for {
		select {
		case evt, ok := <-qrChan:
			if !ok {
				return qrResult(last)
			}

			onEvent(evt)
			last = evt
		case <-timer.C:
			return fmt.Errorf("%w: no result after %s", errQRFailed, deadline)
		}
	}
}

func qrResult(last whatsmeow.QRChannelItem) error {
	switch last.Event {
	case whatsmeow.QRChannelSuccess.Event:
		return nil
	case whatsmeow.QRChannelEventError:
		return fmt.Errorf("%w: %w", errQRFailed, last.Error)
	case "":
		return fmt.Errorf("%w: QR channel closed without events", errQRFailed)
	default:
		return fmt.Errorf("%w: %s", errQRFailed, last.Event)
	}
}

func (b *Bwhatsapp) handleQREvent(evt whatsmeow.QRChannelItem) {
	if evt.Event == whatsmeow.QRChannelEventCode {
		qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)

		return
	}

	b.Log.Infof("QR channel result: %s", evt.Event)
}

// loginResult reports whether evt ends the wait for login, and with which
// error. Connected means success; the others mean the session cannot be used.
// Everything else, including Disconnected during the post-pairing 515
// reconnect, does not end the wait.
func loginResult(evt any) (bool, error) {
	switch e := evt.(type) {
	case *events.Connected:
		return true, nil
	case *events.LoggedOut:
		return true, fmt.Errorf("%w: logged out (reason: %s, on connect: %t)", errLoginFailed, e.Reason, e.OnConnect)
	case *events.StreamReplaced:
		return true, fmt.Errorf("%w: stream replaced by another client", errLoginFailed)
	case *events.ConnectFailure:
		return true, fmt.Errorf("%w: connect failure (%s: %s)", errLoginFailed, e.Reason, e.Message)
	case *events.TemporaryBan:
		return true, fmt.Errorf("%w: %s", errLoginFailed, e.String())
	case *events.ClientOutdated:
		return true, fmt.Errorf("%w: client outdated, whatsmeow needs updating", errLoginFailed)
	}

	return false, nil
}

// signalLogin passes a login result to a waiting Connect. It must never
// block: it runs inside whatsmeow's event dispatch, which holds a lock that
// adding or removing handlers needs.
func (b *Bwhatsapp) signalLogin(err error) {
	ch := b.loginCh.Load()
	if ch == nil {
		return
	}

	select {
	case *ch <- err:
	default:
	}
}

// waitForLogin waits for a login result. If none arrives in time but the
// client is already connected and logged in (Connected is only sent after
// the prekey check and the passive IQ that follow authentication), it
// returns late=true instead of an error.
func waitForLogin(ch <-chan error, timeout time.Duration, usable func() bool) (bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-ch:
		return false, err
	case <-timer.C:
		if usable() {
			return true, nil
		}

		return false, fmt.Errorf("%w after %s", errLoginTimeout, timeout)
	}
}

func (b *Bwhatsapp) awaitLogin(ch <-chan error) error {
	late, err := waitForLogin(ch, loginTimeout, func() bool {
		return b.wc.IsConnected() && b.wc.IsLoggedIn()
	})
	if late {
		b.Log.Warnf("No WhatsApp Connected event after %s, but the client is logged in; continuing", loginTimeout)
	}

	return err
}

// failConnect closes the socket of a client whose Connect failed after it was
// opened, so that a bridge the router disables or gives up on does not stay
// connected. Event handlers are left in place: removing them needs a lock that
// a handler blocked on b.Remote can hold until the router starts reading.
func (b *Bwhatsapp) failConnect(err error) error {
	b.wc.Disconnect()

	return err
}

// loadUsers maps the contacts to users and fetches their avatars.
func (b *Bwhatsapp) loadUsers() {
	for id, contact := range b.contacts {
		if !isGroupJid(id.String()) && id.String() != "status@broadcast" {
			// it is user
			b.users[id.String()] = contact
		}
	}

	b.Log.Info("Getting user avatars..")

	for jid := range b.users {
		info, err := b.GetProfilePicThumb(jid)
		if err != nil {
			b.Log.Warnf("Could not get profile photo of %s: %v", jid, err)
		} else {
			b.Lock()
			if info != nil {
				b.userAvatars[jid] = info.URL
			}
			b.Unlock()
		}
	}

	b.Log.Info("Finished getting avatars..")
}
