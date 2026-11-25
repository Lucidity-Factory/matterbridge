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
// version (405) or failed the connect. whatsmeow does not reconnect by itself
// after either, so without this the bridge stays disconnected while the
// process keeps running. cause names the trigger in the log. At most one
// recovery runs at a time.
func (b *Bwhatsapp) startRecovery(wc recoveryClient, cause string) {
	if !b.recovering.CompareAndSwap(false, true) {
		return
	}

	go b.runRecovery(wc, cause)
}

// runRecovery repeats recovery rounds until one logs in, fails in a way a
// retry cannot fix, or ends in a connect failure maxConnectFailureRounds times.
func (b *Bwhatsapp) runRecovery(wc recoveryClient, cause string) {
	defer b.recovering.Store(false)

	connectFailures := 0

	for {
		retry, err := b.recoveryRound(wc)
		if errors.Is(err, errConnectFailure) {
			connectFailures++
		}

		switch {
		case err == nil:
			b.Log.Infof("WhatsApp reconnected after %s", cause)

			return
		case !retry:
			b.Log.Errorf("WhatsApp reconnect after %s stopped: %v", cause, err)

			return
		case connectFailures >= maxConnectFailureRounds:
			b.Log.Errorf("WhatsApp reconnect after %s gave up after %d connect failures: %v", cause, connectFailures, err)

			return
		case errors.Is(err, errClientOutdated):
			b.Log.Errorf("WhatsApp still rejects client version %s; retrying in %s. whatsmeow may need updating.", store.GetWAVersion(), b.retryInterval())
		default:
			b.Log.Errorf("WhatsApp reconnect after %s failed (%v); retrying in %s", cause, err, b.retryInterval())
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

	return errors.Is(err, errClientOutdated) || errors.Is(err, errLoginTimeout) || errors.Is(err, errConnectFailure), err
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
