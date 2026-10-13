package bwhatsapp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	cfgPresenceMode          = "PresenceMode"
	cfgPresenceBlipSeconds   = "PresenceBlipSeconds"
	cfgPresenceIntervalHours = "PresenceIntervalHours"

	presenceModeOff         = "off"
	presenceModeUnavailable = "unavailable"
	presenceModeBlip        = "blip"

	defaultPresenceBlip = 5 * time.Second
	presenceSendTimeout = 10 * time.Second
	presenceStopTimeout = 2 * time.Second

	// presenceStopSendTimeout bounds an "unavailable" send once the keeper
	// is stopping, so that it fits within presenceStopTimeout.
	presenceStopSendTimeout = 1500 * time.Millisecond
)

// presenceClient is the part of *whatsmeow.Client the presence keeper uses.
// Signatures follow go.mau.fi/whatsmeow e9a033b24933 (presence.go SendPresence,
// client.go IsConnected and IsLoggedIn).
type presenceClient interface {
	SendPresence(ctx context.Context, state types.Presence) error
	IsConnected() bool
	IsLoggedIn() bool
}

// presenceKeeper sends presence updates after every successful login, and
// optionally on an interval, according to the PresenceMode setting.
//
// whatsmeow event handlers run inline, so handleEvent only signals the run
// loop, which does the sending and waiting.
type presenceKeeper struct {
	client   presenceClient
	log      *logrus.Entry
	mode     string
	blip     time.Duration
	interval time.Duration

	trigger chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}

// newPresenceKeeper returns nil when presence updates are disabled.
func newPresenceKeeper(client presenceClient, log *logrus.Entry, mode string, blip, interval time.Duration) *presenceKeeper {
	mode = strings.ToLower(strings.TrimSpace(mode))

	switch mode {
	case "", presenceModeOff:
		return nil
	case presenceModeUnavailable, presenceModeBlip:
	default:
		log.Warnf("Unknown %s %q, presence updates disabled", cfgPresenceMode, mode)

		return nil
	}

	if blip <= 0 {
		blip = defaultPresenceBlip
	}

	if interval < 0 {
		interval = 0
	}

	return &presenceKeeper{
		client:   client,
		log:      log,
		mode:     mode,
		blip:     blip,
		interval: interval,
		trigger:  make(chan struct{}, 1),
	}
}

func (b *Bwhatsapp) newPresenceKeeper(client presenceClient) *presenceKeeper {
	return newPresenceKeeper(
		client,
		b.Log,
		b.GetString(cfgPresenceMode),
		time.Duration(b.GetInt(cfgPresenceBlipSeconds))*time.Second,
		time.Duration(b.GetInt(cfgPresenceIntervalHours))*time.Hour,
	)
}

func (p *presenceKeeper) start() {
	if p == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})

	p.log.Infof("Presence mode %q (blip %s, interval %s)", p.mode, p.blip, p.interval)

	go p.run(ctx)
}

// stop ends the run loop and waits briefly for it, so that an interrupted
// blip can still send "unavailable" before the socket is closed.
func (p *presenceKeeper) stop() {
	if p == nil || p.cancel == nil {
		return
	}

	p.cancel()

	select {
	case <-p.done:
	case <-time.After(presenceStopTimeout):
		p.log.Warn("Presence keeper did not stop in time")
	}
}

// handleEvent is registered as a whatsmeow event handler.
//
// PushNameSetting covers a push name change, but whatsmeow does not emit it
// during a full app state sync, which is how the push name first arrives
// after pairing. That sync ends with AppStateSyncComplete for the
// critical_block patch, which holds the push name; mautrix-whatsapp
// (73b70f27, pkg/connector/handlewhatsapp.go) updates presence on it too.
func (p *presenceKeeper) handleEvent(evt any) {
	switch e := evt.(type) {
	case *events.Connected, *events.PushNameSetting:
		p.signal()
	case *events.AppStateSyncComplete:
		if e.Name == appstate.WAPatchCriticalBlock {
			p.signal()
		}
	}
}

func (p *presenceKeeper) signal() {
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

func (p *presenceKeeper) run(ctx context.Context) {
	defer close(p.done)

	var tick <-chan time.Time

	if p.interval > 0 {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		tick = ticker.C
	}

	for {
		var reason string

		select {
		case <-ctx.Done():
			return
		case <-p.trigger:
			reason = "event"
		case <-tick:
			reason = "interval"
		}

		// select picks at random among ready cases, so a trigger or tick can
		// win over a stop that is already pending.
		if ctx.Err() != nil {
			return
		}

		p.update(ctx, reason)
	}
}

func (p *presenceKeeper) update(ctx context.Context, reason string) {
	// A dropped socket leaves IsLoggedIn true until whatsmeow sees a stream
	// error, so both are checked.
	if !p.client.IsConnected() || !p.client.IsLoggedIn() {
		p.log.Debugf("Skipping presence update (%s): not connected or not logged in", reason)

		return
	}

	if p.mode == presenceModeUnavailable {
		p.sendUnavailable(ctx, reason)

		return
	}

	// whatsmeow switches delivery receipts to "active" as soon as it is asked
	// for "available", before anything is written, and a cancelled send may
	// still reach the server. So "unavailable" follows any attempt, except
	// ErrNoPushName, which whatsmeow returns before changing anything.
	err := p.send(ctx, types.PresenceAvailable, reason)
	if errors.Is(err, whatsmeow.ErrNoPushName) {
		return
	}

	if err == nil {
		select {
		case <-time.After(p.blip):
		case <-ctx.Done():
		}
	}

	p.sendUnavailable(ctx, reason)
}

// sendUnavailable ignores cancellation of the run loop's context, so that
// "unavailable" still goes out while the keeper is stopping, but once the loop
// is cancelled it gives the send at most presenceStopSendTimeout.
func (p *presenceKeeper) sendUnavailable(loop context.Context, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(loop), presenceSendTimeout)
	defer cancel()

	stopAfter := context.AfterFunc(loop, func() {
		time.AfterFunc(presenceStopSendTimeout, cancel)
	})
	defer stopAfter()

	_ = p.send(ctx, types.PresenceUnavailable, reason)
}

func (p *presenceKeeper) send(ctx context.Context, state types.Presence, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, presenceSendTimeout)
	defer cancel()

	err := p.client.SendPresence(ctx, state)
	if err != nil {
		p.log.Warnf("Failed to send presence %s (%s): %v", state, reason, err)

		return err
	}

	p.log.Infof("Sent presence %s (%s)", state, reason)

	return nil
}
