package bwhatsapp

import (
	"context"
	"errors"
	"time"

	"go.mau.fi/whatsmeow/store"
)

// recoveryClient is the part of *whatsmeow.Client that recovery uses.
type recoveryClient interface {
	Connect() error
	Disconnect()
}

// recoveryTarget is the client a recovery reconnects.
func (b *Bwhatsapp) recoveryTarget() recoveryClient {
	if b.testRecoveryClient != nil {
		return b.testRecoveryClient
	}

	return b.wc
}

// startRecovery reconnects a running bridge after WhatsApp rejected its client
// version (405). whatsmeow does not reconnect by itself after a 405, so
// without this the bridge stays disconnected while the process keeps running.
// At most one recovery runs at a time.
func (b *Bwhatsapp) startRecovery(wc recoveryClient) {
	if !b.recovering.CompareAndSwap(false, true) {
		return
	}

	go b.runRecovery(wc)
}

// runRecovery repeats recovery rounds until one logs in or fails in a way a
// retry cannot fix.
func (b *Bwhatsapp) runRecovery(wc recoveryClient) {
	defer b.recovering.Store(false)

	for {
		retry, err := b.recoveryRound(wc)

		switch {
		case err == nil:
			b.Log.Info("WhatsApp reconnected after client outdated")

			return
		case !retry:
			b.Log.Errorf("WhatsApp reconnect after client outdated stopped: %v", err)

			return
		case errors.Is(err, errClientOutdated):
			b.Log.Errorf("WhatsApp still rejects client version %s; retrying in %s. whatsmeow may need updating.", store.GetWAVersion(), b.retryInterval())
		default:
			b.Log.Errorf("WhatsApp reconnect after client outdated failed (%v); retrying in %s", err, b.retryInterval())
		}

		b.recoverySleep(b.retryInterval())
	}
}

// recoveryRound refreshes the client version, reconnects the same client and
// waits for the login result. The bool reports whether a retry can help.
func (b *Bwhatsapp) recoveryRound(wc recoveryClient) (bool, error) {
	version, err := refreshWAVersion(context.Background())
	if err != nil {
		b.Log.Warnf("Could not refresh the WhatsApp web version, trying %s: %v", version, err)
	}

	// The server may not have closed the rejected socket yet; Connect would
	// then fail with ErrAlreadyConnected.
	wc.Disconnect()

	ch, disarm := b.armLogin()
	defer disarm()

	err = wc.Connect()
	if err != nil {
		wc.Disconnect()

		return true, err
	}

	// Not awaitLogin: whatsmeow keeps IsLoggedIn true after a 405, so its
	// late-login check would take a stale login for success.
	_, err = waitForLogin(ch, b.loginWait(), func() bool { return false })
	if err == nil {
		return false, nil
	}

	// A login after the wait must not leave a half-open connection behind.
	wc.Disconnect()

	return errors.Is(err, errClientOutdated) || errors.Is(err, errLoginTimeout), err
}

func (b *Bwhatsapp) retryInterval() time.Duration {
	if b.recoveryRetry > 0 {
		return b.recoveryRetry
	}

	return recoveryRetryInterval
}

func (b *Bwhatsapp) loginWait() time.Duration {
	if b.recoveryLoginWait > 0 {
		return b.recoveryLoginWait
	}

	return loginTimeout
}

func (b *Bwhatsapp) recoverySleep(d time.Duration) {
	if b.sleepFn != nil {
		b.sleepFn(d)

		return
	}

	time.Sleep(d)
}
