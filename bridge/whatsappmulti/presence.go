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
	cfgPresenceActiveMinutes = "PresenceActiveMinutes"

	presenceModeOff         = "off"
	presenceModeUnavailable = "unavailable"
	presenceModeBlip        = "blip"
	presenceModeAvailable   = "available"
	presenceModeActive      = "active"

	defaultPresenceBlip   = 5 * time.Second
	defaultPresenceActive = 5 * time.Minute
	presenceSendTimeout   = 10 * time.Second
	presenceStopTimeout   = 2 * time.Second

	// presenceStopSendTimeout bounds an "unavailable" send once the keeper
	// is stopping, so that it fits within presenceStopTimeout.
	presenceStopSendTimeout = 1500 * time.Millisecond

	// Reasons shown in the presence log lines.
	reasonEvent    = "event"
	reasonInterval = "interval"
	reasonActivity = "activity"
	reasonIdle     = "idle"
	reasonStop     = "stop"
)

// presenceClient is the part of *whatsmeow.Client the presence keeper uses.
// Signatures follow go.mau.fi/whatsmeow e9a033b24933 (presence.go SendPresence,
// client.go IsConnected and IsLoggedIn).
type presenceClient interface {
	SendPresence(ctx context.Context, state types.Presence) error
	IsConnected() bool
	IsLoggedIn() bool
}

// presenceKeeper sends presence updates according to the PresenceMode
// setting: after every successful login, on push name changes, optionally on
// an interval, and in active mode after every message the bridge sends.
//
// whatsmeow event handlers and the bridge's Send run on other goroutines, so
// they only signal the run loop, which does the sending. The loop alone owns
// the online state: blip and active modes go online until a deadline, and the
// timer for that deadline sends "unavailable".
type presenceKeeper struct {
	client   presenceClient
	log      *logrus.Entry
	mode     string
	blip     time.Duration
	interval time.Duration
	active   time.Duration

	trigger  chan struct{}
	activity chan struct{}
	cancel   context.CancelFunc
	done     chan struct{}

	// Owned by the run loop.
	online bool
	until  time.Time
	timer  *time.Timer
}

// newPresenceKeeper returns nil when presence updates are disabled.
func newPresenceKeeper(client presenceClient, log *logrus.Entry, mode string, blip, interval time.Duration) *presenceKeeper {
	mode = strings.ToLower(strings.TrimSpace(mode))

	switch mode {
	case "", presenceModeOff:
		return nil
	case presenceModeUnavailable, presenceModeBlip, presenceModeAvailable, presenceModeActive:
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

	// An idle connection in active mode is left to age out.
	if mode == presenceModeActive && interval > 0 {
		log.Infof("%s is ignored in active mode", cfgPresenceIntervalHours)

		interval = 0
	}

	return &presenceKeeper{
		client:   client,
		log:      log,
		mode:     mode,
		blip:     blip,
		interval: interval,
		active:   defaultPresenceActive,
		trigger:  make(chan struct{}, 1),
		activity: make(chan struct{}, 1),
	}
}

// setActive sets how long active mode stays online after the last send. A
// value of 0 or less keeps the default.
func (p *presenceKeeper) setActive(active time.Duration) {
	if p == nil || active <= 0 {
		return
	}

	p.active = active
}

func (b *Bwhatsapp) newPresenceKeeper(client presenceClient) *presenceKeeper {
	p := newPresenceKeeper(
		client,
		b.Log,
		b.GetString(cfgPresenceMode),
		time.Duration(b.GetInt(cfgPresenceBlipSeconds))*time.Second,
		time.Duration(b.GetInt(cfgPresenceIntervalHours))*time.Hour,
	)
	p.setActive(time.Duration(b.GetInt(cfgPresenceActiveMinutes)) * time.Minute)

	return p
}

func (p *presenceKeeper) start() {
	if p == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})

	p.log.Infof("Presence mode %q (blip %s, interval %s, active %s)", p.mode, p.blip, p.interval, p.active)

	if p.mode == presenceModeAvailable {
		p.warnAvailable()
	}

	go p.run(ctx)
}

// warnAvailable warns when available mode has no interval: a send that fails
// while the socket stays up is then not retried until the next login or push
// name change.
func (p *presenceKeeper) warnAvailable() {
	if p.interval != 0 {
		return
	}

	p.log.Warnf("PresenceMode available: set %s so a failed send is retried while connected", cfgPresenceIntervalHours)
}

// stop ends the run loop and waits briefly for it, so that a bridge that is
// online can still send "unavailable" before the socket is closed.
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

// markActive records that the bridge sent a message to WhatsApp. Only active
// mode reacts to it.
func (p *presenceKeeper) markActive() {
	if p == nil || p.mode != presenceModeActive {
		return
	}

	select {
	case p.activity <- struct{}{}:
	default:
	}
}

func (p *presenceKeeper) run(ctx context.Context) {
	defer close(p.done)
	defer p.clear()

	var tick <-chan time.Time

	if p.interval > 0 {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		tick = ticker.C
	}

	for {
		var handle func(context.Context)

		select {
		case <-ctx.Done():
		case <-p.trigger:
			handle = p.onTrigger
		case <-tick:
			handle = p.onTick
		case <-p.activity:
			handle = p.onActivity
		case <-p.timerC():
			handle = p.onTimer
		}

		// select picks at random among ready cases, so another input can win
		// over a stop that is already pending.
		if ctx.Err() != nil {
			p.onStop(ctx)

			return
		}

		handle(ctx)
	}
}

func (p *presenceKeeper) onTrigger(ctx context.Context) {
	p.refresh(ctx, reasonEvent)
}

func (p *presenceKeeper) onTick(ctx context.Context) {
	p.refresh(ctx, reasonInterval)
}

// refresh handles a login, a push name change or an interval tick. A bridge
// that is already online sends "available" again, since a reconnect may have
// reset it on the server, and keeps its deadline.
func (p *presenceKeeper) refresh(ctx context.Context, reason string) {
	if !p.connected(reason) {
		return
	}

	switch p.mode {
	case presenceModeUnavailable:
		p.sendUnavailable(ctx, reason)
	case presenceModeAvailable:
		_ = p.send(ctx, types.PresenceAvailable, reason)
	case presenceModeBlip:
		if p.online {
			p.resend(ctx, reason)
		} else {
			p.goOnline(ctx, p.blip, reason)
		}
	case presenceModeActive:
		if p.online {
			p.resend(ctx, reason)
		} else {
			p.sendUnavailable(ctx, reason)
		}
	}
}

// onActivity extends an online bridge, even while disconnected, and takes an
// offline one online.
func (p *presenceKeeper) onActivity(ctx context.Context) {
	if p.mode != presenceModeActive {
		return
	}

	if p.online {
		p.extend(p.active)

		return
	}

	if p.connected(reasonActivity) {
		p.goOnline(ctx, p.active, reasonActivity)
	}
}

func (p *presenceKeeper) onTimer(ctx context.Context) {
	// A send that arrived together with the deadline keeps the bridge online.
	select {
	case <-p.activity:
		p.extend(p.active)

		return
	default:
	}

	p.goOffline(ctx, reasonIdle)
}

func (p *presenceKeeper) onStop(ctx context.Context) {
	if p.online {
		p.goOffline(ctx, reasonStop)
	}
}

// A dropped socket leaves IsLoggedIn true until whatsmeow sees a stream
// error, so both are checked.
func (p *presenceKeeper) connected(reason string) bool {
	if p.client.IsConnected() && p.client.IsLoggedIn() {
		return true
	}

	p.log.Debugf("Skipping presence update (%s): not connected or not logged in", reason)

	return false
}

// goOnline sends "available" and stays online for d.
//
// whatsmeow switches delivery receipts to "active" as soon as it is asked for
// "available", before anything is written, and a cancelled send may still
// reach the server. So "unavailable" follows any failed attempt, except
// ErrNoPushName, which whatsmeow returns before changing anything.
func (p *presenceKeeper) goOnline(ctx context.Context, d time.Duration, reason string) {
	err := p.send(ctx, types.PresenceAvailable, reason)

	switch {
	case err == nil:
		p.online = true
		p.until = time.Now().Add(d)
		p.armTimer()
	case !errors.Is(err, whatsmeow.ErrNoPushName):
		p.sendUnavailable(ctx, reason)
	}
}

// resend sends "available" again while online. On failure it goes offline,
// for the reason given at goOnline.
func (p *presenceKeeper) resend(ctx context.Context, reason string) {
	err := p.send(ctx, types.PresenceAvailable, reason)
	if err == nil || errors.Is(err, whatsmeow.ErrNoPushName) {
		return
	}

	p.clear()
	p.sendUnavailable(ctx, reason)
}

// extend moves the deadline to d from now.
func (p *presenceKeeper) extend(d time.Duration) {
	p.until = time.Now().Add(d)
	p.armTimer()
}

func (p *presenceKeeper) goOffline(ctx context.Context, reason string) {
	p.clear()

	if p.connected(reason) {
		p.sendUnavailable(ctx, reason)
	}
}

func (p *presenceKeeper) armTimer() {
	wait := time.Until(p.until)

	if p.timer == nil {
		p.timer = time.NewTimer(wait)

		return
	}

	p.timer.Reset(wait)
}

func (p *presenceKeeper) clear() {
	p.online = false
	p.until = time.Time{}

	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

func (p *presenceKeeper) timerC() <-chan time.Time {
	if p.timer == nil {
		return nil
	}

	return p.timer.C
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
