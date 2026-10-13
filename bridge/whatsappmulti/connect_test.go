package bwhatsapp

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

var (
	errTestBoom      = errors.New("boom")
	errTestLoggedOut = errors.New("logged out")
	errTestSecond    = errors.New("second")
)

func TestLoginResult(t *testing.T) {
	cases := []struct {
		name    string
		evt     any
		done    bool
		wantErr string // empty: no error expected
	}{
		{"Connected", &events.Connected{}, true, ""},
		{"LoggedOut", &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, true, "logged out from another device"},
		{"StreamReplaced", &events.StreamReplaced{}, true, "stream replaced"},
		{"ConnectFailure", &events.ConnectFailure{Reason: events.ConnectFailureServiceUnavailable, Message: "busy"}, true, "busy"},
		{"TemporaryBan", &events.TemporaryBan{Code: events.TempBanSentToTooManyPeople}, true, "temporarily banned"},
		{"ClientOutdated", &events.ClientOutdated{}, true, "client outdated"},
		// Not terminal: these happen during a normal first pairing or later.
		{"Disconnected", &events.Disconnected{}, false, ""},
		{"KeepAliveTimeout", &events.KeepAliveTimeout{}, false, ""},
		{"PairSuccess", &events.PairSuccess{}, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done, err := loginResult(tc.evt)

			assert.Equal(t, tc.done, done)

			if tc.wantErr == "" {
				assert.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.ErrorIs(t, err, errLoginFailed)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func qrItems(items ...whatsmeow.QRChannelItem) <-chan whatsmeow.QRChannelItem {
	ch := make(chan whatsmeow.QRChannelItem, len(items))
	for _, item := range items {
		ch <- item
	}

	close(ch)

	return ch
}

func code() whatsmeow.QRChannelItem {
	return whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "2@fake"}
}

func TestWaitForQR(t *testing.T) {
	cases := []struct {
		name    string
		items   []whatsmeow.QRChannelItem
		wantErr string // empty: success expected
		wraps   error
	}{
		{"success", []whatsmeow.QRChannelItem{code(), code(), whatsmeow.QRChannelSuccess}, "", nil},
		{"qr timeout", []whatsmeow.QRChannelItem{code(), whatsmeow.QRChannelTimeout}, whatsmeow.QRChannelTimeout.Event, nil},
		{"error", []whatsmeow.QRChannelItem{code(), {Event: whatsmeow.QRChannelEventError, Error: errTestBoom}}, "boom", errTestBoom},
		{"intermediate error then timeout", []whatsmeow.QRChannelItem{whatsmeow.QRChannelScannedWithoutMultidevice, code(), whatsmeow.QRChannelTimeout}, whatsmeow.QRChannelTimeout.Event, nil},
		{"closed after a code", []whatsmeow.QRChannelItem{code()}, "code", nil},
		{"closed without events", nil, "without events", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string

			err := waitForQR(qrItems(tc.items...), time.Second, func(evt whatsmeow.QRChannelItem) {
				seen = append(seen, evt.Event)
			})

			assert.Len(t, seen, len(tc.items))

			if tc.wantErr == "" {
				assert.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.ErrorIs(t, err, errQRFailed)
			assert.Contains(t, err.Error(), tc.wantErr)

			if tc.wraps != nil {
				assert.ErrorIs(t, err, tc.wraps)
			}
		})
	}
}

func TestWaitForQRDeadline(t *testing.T) {
	never := make(chan whatsmeow.QRChannelItem)

	err := waitForQR(never, 20*time.Millisecond, func(whatsmeow.QRChannelItem) {})

	require.Error(t, err)
	assert.ErrorIs(t, err, errQRFailed)
	assert.Contains(t, err.Error(), "no result after")
}

// runWaitForLogin runs waitForLogin with the test's own deadline, so that a
// waitForLogin that never returns fails the test instead of hanging it.
func runWaitForLogin(t *testing.T, ch <-chan error, timeout time.Duration, usable bool) (bool, error) {
	t.Helper()

	type result struct {
		late bool
		err  error
	}

	out := make(chan result, 1)

	go func() {
		late, err := waitForLogin(ch, timeout, func() bool { return usable })
		out <- result{late, err}
	}()

	select {
	case r := <-out:
		return r.late, r.err
	case <-time.After(2 * time.Second):
		t.Fatal("waitForLogin did not return")

		return false, nil
	}
}

func TestWaitForLogin(t *testing.T) {
	t.Run("connected", func(t *testing.T) {
		ch := make(chan error, 1)
		ch <- nil

		late, err := runWaitForLogin(t, ch, time.Second, false)

		assert.False(t, late)
		assert.NoError(t, err)
	})

	t.Run("failure is returned as is", func(t *testing.T) {
		ch := make(chan error, 1)
		ch <- errTestLoggedOut

		late, err := runWaitForLogin(t, ch, time.Second, true)

		assert.False(t, late)
		assert.Equal(t, errTestLoggedOut, err)
	})

	t.Run("timeout while not usable", func(t *testing.T) {
		late, err := runWaitForLogin(t, make(chan error), 20*time.Millisecond, false)

		assert.False(t, late)
		require.Error(t, err)
		assert.ErrorIs(t, err, errLoginTimeout)
	})

	t.Run("timeout while usable continues", func(t *testing.T) {
		late, err := runWaitForLogin(t, make(chan error), 20*time.Millisecond, true)

		assert.True(t, late)
		assert.NoError(t, err)
	})
}

func TestSignalLoginNeverBlocks(t *testing.T) {
	b := &Bwhatsapp{}

	done := make(chan struct{})

	go func() {
		// No Connect waiting: a nil channel pointer is a no-op.
		b.signalLogin(nil)

		// A waiting Connect that already has a result: the second signal
		// is dropped, not blocked on.
		ch := make(chan error, 1)
		b.loginCh.Store(&ch)
		b.signalLogin(nil)
		b.signalLogin(errTestSecond)

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("signalLogin blocked")
	}

	ch := *b.loginCh.Load()
	require.Len(t, ch, 1)
	assert.NoError(t, <-ch, "the first result is kept")
}
