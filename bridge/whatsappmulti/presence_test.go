package bwhatsapp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matterbridge-org/matterbridge/bridge"
	"github.com/matterbridge-org/matterbridge/bridge/config"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// fakePresenceClient stands in for *whatsmeow.Client, matching the signatures
// of SendPresence (presence.go), IsConnected and IsLoggedIn (client.go) at
// whatsmeow e9a033b24933. SendPresence follows the real order of checks:
// ErrNoPushName (presence.go:67-68) and ErrNotConnected (client.go sendNode,
// errors.go:22) come before anything is written, and NoiseSocket.SendFrame
// (socket/noisesocket.go:91-110) returns ctx.Err() for a cancelled context.
// Only a send that gets past these checks is recorded.
//
// pushName, connected, loggedIn, errs, block and blockState are set before
// the keeper starts and are not changed afterwards.
type fakePresenceClient struct {
	pushName  bool
	connected bool
	loggedIn  bool
	errs      map[types.Presence]error

	// Sends of blockState wait for block to be closed (or the context to
	// end); blocked is signalled when such a send starts waiting.
	block      chan struct{}
	blockState types.Presence
	blocked    chan struct{}

	mu    sync.Mutex
	sent  []types.Presence
	at    []time.Time
	calls chan types.Presence
}

func newFakePresenceClient(loggedIn bool) *fakePresenceClient {
	return &fakePresenceClient{
		pushName:  true,
		connected: true,
		loggedIn:  loggedIn,
		errs:      map[types.Presence]error{},
		blocked:   make(chan struct{}, 16),
		calls:     make(chan types.Presence, 16),
	}
}

func (f *fakePresenceClient) SendPresence(ctx context.Context, state types.Presence) error {
	if !f.pushName {
		return whatsmeow.ErrNoPushName
	}

	if !f.connected {
		return whatsmeow.ErrNotConnected
	}

	if f.block != nil && state == f.blockState {
		f.blocked <- struct{}{}

		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	err := ctx.Err()
	if err != nil {
		return err
	}

	f.mu.Lock()
	f.sent = append(f.sent, state)
	f.at = append(f.at, time.Now())
	err = f.errs[state]
	f.mu.Unlock()

	f.calls <- state

	return err
}

func (f *fakePresenceClient) IsConnected() bool {
	return f.connected
}

func (f *fakePresenceClient) IsLoggedIn() bool {
	return f.loggedIn
}

func (f *fakePresenceClient) sentStates() []types.Presence {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]types.Presence(nil), f.sent...)
}

func (f *fakePresenceClient) waitCall(t *testing.T) types.Presence {
	t.Helper()

	select {
	case state := <-f.calls:
		return state
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SendPresence")

		return ""
	}
}

func (f *fakePresenceClient) waitBlocked(t *testing.T) {
	t.Helper()

	select {
	case <-f.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a blocked SendPresence")
	}
}

func (f *fakePresenceClient) expectNoCall(t *testing.T, wait time.Duration) {
	t.Helper()

	select {
	case state := <-f.calls:
		t.Fatalf("unexpected SendPresence(%s)", state)
	case <-time.After(wait):
	}
}

func countEntries(hook *logrustest.Hook, level logrus.Level, substr string) int {
	n := 0

	for _, entry := range hook.AllEntries() {
		if entry.Level == level && strings.Contains(entry.Message, substr) {
			n++
		}
	}

	return n
}

func TestPresenceKeeperDisabledModes(t *testing.T) {
	for _, mode := range []string{"", "off", " OFF ", "online"} {
		log, hook := newTestLogger()
		p := newPresenceKeeper(newFakePresenceClient(true), log, mode, time.Second, 0)
		assert.Nil(t, p, "mode %q", mode)

		// Only an unknown mode is worth a warning; the forms of "off" are not.
		wantWarnings := 0
		if mode == "online" {
			wantWarnings = 1
		}

		assert.Equal(t, wantWarnings, countEntries(hook, logrus.WarnLevel, ""), "mode %q", mode)
	}

	// A nil keeper is what Connect and Disconnect hold when disabled.
	var p *presenceKeeper

	assert.NotPanics(t, func() {
		p.start()
		p.stop()
	})
}

func TestPresenceKeeperUnknownModeWarns(t *testing.T) {
	log, hook := newTestLogger()
	newPresenceKeeper(newFakePresenceClient(true), log, "online", time.Second, 0)

	require.NotNil(t, hook.LastEntry())
	assert.Equal(t, logrus.WarnLevel, hook.LastEntry().Level)
	assert.Contains(t, hook.LastEntry().Message, `"online"`)
}

func TestPresenceKeeperDefaults(t *testing.T) {
	log, _ := newTestLogger()
	p := newPresenceKeeper(newFakePresenceClient(true), log, "Blip", 0, -time.Hour)

	require.NotNil(t, p)
	assert.Equal(t, "blip", p.mode)
	assert.Equal(t, 5*time.Second, p.blip)
	assert.Equal(t, time.Duration(0), p.interval)
}

func TestPresenceKeeperUnavailableOnLogin(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.Connected{})

	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
	client.expectNoCall(t, 50*time.Millisecond)
}

func TestPresenceKeeperBlipOnLogin(t *testing.T) {
	const blip = 100 * time.Millisecond

	client := newFakePresenceClient(true)
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "blip", blip, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.Connected{})

	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))

	client.mu.Lock()
	gap := client.at[1].Sub(client.at[0])
	client.mu.Unlock()
	assert.GreaterOrEqual(t, gap, blip, "available must be held for the blip duration")
}

func TestPresenceKeeperPushNameSettingTriggers(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.PushNameSetting{})

	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
}

func TestPresenceKeeperCriticalBlockSyncTriggers(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 0)

	p.start()
	defer p.stop()

	// The full sync after pairing delivers the push name without a
	// PushNameSetting event, only this.
	p.handleEvent(&events.AppStateSyncComplete{Name: appstate.WAPatchCriticalBlock})

	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
}

func TestPresenceKeeperIgnoresOtherEvents(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.Disconnected{})
	p.handleEvent(&events.Message{})
	p.handleEvent(&events.AppStateSyncComplete{Name: appstate.WAPatchRegular})

	client.expectNoCall(t, 50*time.Millisecond)
}

func TestPresenceKeeperConfigUnits(t *testing.T) {
	log, _ := newTestLogger()
	input := []byte(`
[whatsapp.test]
PresenceMode="blip"
PresenceBlipSeconds=7
PresenceIntervalHours=3
`)
	b := &Bwhatsapp{Config: &bridge.Config{Bridge: &bridge.Bridge{
		Account: "whatsapp.test",
		Config:  config.NewConfigFromString(log.Logger, input),
		Log:     log,
	}}}

	p := b.newPresenceKeeper(newFakePresenceClient(true))

	require.NotNil(t, p)
	assert.Equal(t, "blip", p.mode)
	assert.Equal(t, 7*time.Second, p.blip)
	assert.Equal(t, 3*time.Hour, p.interval)
}

func TestPresenceKeeperSkipsWhenNotConnectedOrNotLoggedIn(t *testing.T) {
	cases := []struct {
		name      string
		connected bool
		loggedIn  bool
	}{
		{"connected, not logged in", true, false},
		{"logged in, not connected", false, true},
	}

	for _, tc := range cases {
		client := newFakePresenceClient(tc.loggedIn)
		client.connected = tc.connected
		log, hook := newTestLogger()
		log.Logger.SetLevel(logrus.DebugLevel)
		p := newPresenceKeeper(client, log, "blip", 10*time.Millisecond, 0)

		p.update(context.Background(), "event")

		assert.Empty(t, client.sentStates(), tc.name)
		assert.Equal(t, 1, countEntries(hook, logrus.DebugLevel, "Skipping presence update (event)"), tc.name)
		assert.Zero(t, countEntries(hook, logrus.WarnLevel, ""), tc.name)
	}
}

func TestPresenceKeeperHandleEventDoesNotBlock(t *testing.T) {
	log, _ := newTestLogger()
	// Not started: nothing drains the trigger channel.
	p := newPresenceKeeper(newFakePresenceClient(true), log, "blip", time.Second, 0)

	done := make(chan struct{})

	go func() {
		for range 5 {
			p.handleEvent(&events.Connected{})
		}

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleEvent blocked")
	}
}

func TestPresenceKeeperNoPushName(t *testing.T) {
	client := newFakePresenceClient(true)
	client.pushName = false
	log, hook := newTestLogger()
	p := newPresenceKeeper(client, log, "blip", 10*time.Millisecond, 0)

	p.update(context.Background(), "event")

	// whatsmeow refuses before changing anything, so there is no
	// "unavailable" to follow up with: one warning, nothing sent.
	assert.Empty(t, client.sentStates())
	assert.Equal(t, 1, countEntries(hook, logrus.WarnLevel, whatsmeow.ErrNoPushName.Error()))
}

func TestPresenceKeeperAvailableFailureStillSendsUnavailable(t *testing.T) {
	client := newFakePresenceClient(true)
	client.errs[types.PresenceAvailable] = whatsmeow.ErrNotConnected
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "blip", time.Hour, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.Connected{})

	// The hour-long blip is skipped: "unavailable" follows straight away.
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
}

func TestPresenceKeeperStopDuringAvailableSend(t *testing.T) {
	client := newFakePresenceClient(true)
	client.block = make(chan struct{})
	client.blockState = types.PresenceAvailable
	log, hook := newTestLogger()
	p := newPresenceKeeper(client, log, "blip", time.Hour, 0)

	p.start()

	p.handleEvent(&events.Connected{})
	client.waitBlocked(t)

	p.stop()

	assert.Equal(t, []types.Presence{types.PresenceUnavailable}, client.sentStates())
	assert.Zero(t, countEntries(hook, logrus.WarnLevel, "did not stop in time"))
}

func TestPresenceKeeperStopDuringUnavailableSend(t *testing.T) {
	client := newFakePresenceClient(true)
	client.block = make(chan struct{})
	client.blockState = types.PresenceUnavailable
	log, hook := newTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 0)

	p.start()

	p.handleEvent(&events.Connected{})
	client.waitBlocked(t)

	p.stop()

	// The send is cut short once the keeper stops, within stop's own wait.
	assert.Empty(t, client.sentStates())
	assert.Equal(t, 1, countEntries(hook, logrus.WarnLevel, "Failed to send presence unavailable"))
	assert.Zero(t, countEntries(hook, logrus.WarnLevel, "did not stop in time"))
}

func TestPresenceKeeperPendingStopWinsOverTrigger(t *testing.T) {
	// select chooses at random between ready cases, so repeat: without the
	// check in run, a send gets through with probability 1 - 2^-20.
	for range 20 {
		client := newFakePresenceClient(true)
		log, _ := newTestLogger()
		p := newPresenceKeeper(client, log, "unavailable", time.Second, 0)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		p.trigger <- struct{}{}

		p.done = make(chan struct{})
		p.run(ctx)

		require.Empty(t, client.sentStates())
	}
}

func TestPresenceKeeperBurstOfTriggersCollapses(t *testing.T) {
	client := newFakePresenceClient(true)
	client.block = make(chan struct{})
	client.blockState = types.PresenceUnavailable
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.Connected{})
	client.waitBlocked(t)

	for range 5 {
		p.handleEvent(&events.PushNameSetting{})
	}

	close(client.block)

	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
	client.expectNoCall(t, 100*time.Millisecond)
}

func TestPresenceKeeperStopMidBlipSendsUnavailable(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "blip", time.Hour, 0)

	p.start()

	p.handleEvent(&events.Connected{})
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	started := time.Now()

	p.stop()

	assert.Less(t, time.Since(started), presenceStopTimeout, "stop must not wait for the blip or time out")
	assert.Equal(t, []types.Presence{types.PresenceAvailable, types.PresenceUnavailable}, client.sentStates())
}

func TestPresenceKeeperInterval(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 30*time.Millisecond)

	p.start()
	defer p.stop()

	// No login event: only the ticker can cause these.
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
}

func TestPresenceKeeperStopWithoutStart(t *testing.T) {
	log, _ := newTestLogger()
	p := newPresenceKeeper(newFakePresenceClient(true), log, "blip", time.Second, 0)

	require.NotNil(t, p)
	assert.NotPanics(t, p.stop)
}

// availableBlip is short so that available mode wrongly taking the blip path
// sends "unavailable" well within the tests' waits.
const availableBlip = 10 * time.Millisecond

var errSendFailed = errors.New("send failed")

func TestPresenceKeeperAvailableModeAccepted(t *testing.T) {
	log, _ := newTestLogger()
	p := newPresenceKeeper(newFakePresenceClient(true), log, "available", availableBlip, 0)

	require.NotNil(t, p)
	assert.Equal(t, "available", p.mode)
}

func TestPresenceKeeperAvailableOnLogin(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newTestLogger()
	p := newPresenceKeeper(client, log, "available", availableBlip, 0)

	p.start()

	p.handleEvent(&events.Connected{})

	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))
	client.expectNoCall(t, 200*time.Millisecond)

	p.stop()

	assert.Equal(t, []types.Presence{types.PresenceAvailable}, client.sentStates())
}

func TestPresenceKeeperAvailableFailureSendsNothingElse(t *testing.T) {
	client := newFakePresenceClient(true)
	client.errs[types.PresenceAvailable] = errSendFailed
	log, hook := newTestLogger()
	p := newPresenceKeeper(client, log, "available", availableBlip, 0)

	p.start()

	p.handleEvent(&events.Connected{})

	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))
	client.expectNoCall(t, 200*time.Millisecond)

	p.stop()

	assert.Equal(t, []types.Presence{types.PresenceAvailable}, client.sentStates())
	assert.Equal(t, 1, countEntries(hook, logrus.WarnLevel, "Failed to send presence available"))
}

func TestPresenceKeeperAvailableStartWarns(t *testing.T) {
	cases := []struct {
		mode        string
		interval    time.Duration
		pushWarns   int
		intervalTip int
	}{
		{mode: "available", interval: 0, pushWarns: 1, intervalTip: 1},
		{mode: "available", interval: time.Hour, pushWarns: 1, intervalTip: 0},
		{mode: "blip", interval: 0, pushWarns: 0, intervalTip: 0},
	}

	for _, tc := range cases {
		log, hook := newTestLogger()
		p := newPresenceKeeper(newFakePresenceClient(true), log, tc.mode, availableBlip, tc.interval)

		p.start()
		p.stop()

		assert.Equal(t, tc.pushWarns, countEntries(hook, logrus.WarnLevel, "push notifications"),
			"mode %s, interval %s", tc.mode, tc.interval)
		assert.Equal(t, tc.intervalTip, countEntries(hook, logrus.WarnLevel, "set PresenceIntervalHours"),
			"mode %s, interval %s", tc.mode, tc.interval)
	}
}

func TestPresenceKeeperAvailableStopSendsNothing(t *testing.T) {
	client := newFakePresenceClient(true)
	log, hook := newTestLogger()
	p := newPresenceKeeper(client, log, "available", time.Hour, 0)

	p.start()

	p.handleEvent(&events.Connected{})
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	p.stop()

	assert.Equal(t, []types.Presence{types.PresenceAvailable}, client.sentStates())
	assert.Zero(t, countEntries(hook, logrus.WarnLevel, "did not stop in time"))
}
