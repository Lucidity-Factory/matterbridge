package bwhatsapp

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
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

func newPresenceTestLogger() (*logrus.Entry, *logrustest.Hook) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	hook := logrustest.NewLocal(logger)

	return logrus.NewEntry(logger), hook
}

// fakePresenceClient stands in for *whatsmeow.Client, matching the signatures
// of SendPresence (presence.go), IsConnected and IsLoggedIn (client.go) at
// whatsmeow e9a033b24933. SendPresence follows the real order of checks:
// ErrNoPushName (presence.go:67-68) and ErrNotConnected (client.go sendNode,
// errors.go:22) come before anything is written, and NoiseSocket.SendFrame
// (socket/noisesocket.go:91-110) returns ctx.Err() for a cancelled context.
// Only a send that gets past these checks is recorded.
//
// pushName, loggedIn, block and blockState are set before the keeper starts
// and are not changed afterwards. connected and errs may change while it runs,
// through connected.Store and setErr.
type fakePresenceClient struct {
	pushName  bool
	connected atomic.Bool
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
	f := &fakePresenceClient{
		pushName: true,
		loggedIn: loggedIn,
		errs:     map[types.Presence]error{},
		blocked:  make(chan struct{}, 16),
		calls:    make(chan types.Presence, 64),
	}
	f.connected.Store(true)

	return f
}

func (f *fakePresenceClient) SendPresence(ctx context.Context, state types.Presence) error {
	if !f.pushName {
		return whatsmeow.ErrNoPushName
	}

	if !f.connected.Load() {
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
	return f.connected.Load()
}

func (f *fakePresenceClient) IsLoggedIn() bool {
	return f.loggedIn
}

func (f *fakePresenceClient) setErr(state types.Presence, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.errs[state] = err
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
		log, hook := newPresenceTestLogger()
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
	log, hook := newPresenceTestLogger()
	newPresenceKeeper(newFakePresenceClient(true), log, "online", time.Second, 0)

	require.NotNil(t, hook.LastEntry())
	assert.Equal(t, logrus.WarnLevel, hook.LastEntry().Level)
	assert.Contains(t, hook.LastEntry().Message, `"online"`)
}

func TestPresenceKeeperDefaults(t *testing.T) {
	log, _ := newPresenceTestLogger()
	p := newPresenceKeeper(newFakePresenceClient(true), log, "Blip", 0, -time.Hour)

	require.NotNil(t, p)
	assert.Equal(t, "blip", p.mode)
	assert.Equal(t, 5*time.Second, p.blip)
	assert.Equal(t, time.Duration(0), p.interval)
}

func TestPresenceKeeperUnavailableOnLogin(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newPresenceTestLogger()
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
	log, _ := newPresenceTestLogger()
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
	log, _ := newPresenceTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.PushNameSetting{})

	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
}

func TestPresenceKeeperCriticalBlockSyncTriggers(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newPresenceTestLogger()
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
	log, _ := newPresenceTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.Disconnected{})
	p.handleEvent(&events.Message{})
	p.handleEvent(&events.AppStateSyncComplete{Name: appstate.WAPatchRegular})

	client.expectNoCall(t, 50*time.Millisecond)
}

func TestPresenceKeeperConfigUnits(t *testing.T) {
	log, _ := newPresenceTestLogger()
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
		client.connected.Store(tc.connected)

		log, hook := newPresenceTestLogger()
		log.Logger.SetLevel(logrus.DebugLevel)
		p := newPresenceKeeper(client, log, "blip", 10*time.Millisecond, 0)

		p.onTrigger(context.Background())

		assert.Empty(t, client.sentStates(), tc.name)
		assert.Equal(t, 1, countEntries(hook, logrus.DebugLevel, "Skipping presence update (event)"), tc.name)
		assert.Zero(t, countEntries(hook, logrus.WarnLevel, ""), tc.name)
	}
}

func TestPresenceKeeperHandleEventDoesNotBlock(t *testing.T) {
	log, _ := newPresenceTestLogger()
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
	log, hook := newPresenceTestLogger()
	p := newPresenceKeeper(client, log, "blip", 10*time.Millisecond, 0)

	p.onTrigger(context.Background())

	// whatsmeow refuses before changing anything, so there is no
	// "unavailable" to follow up with: one warning, nothing sent.
	assert.Empty(t, client.sentStates())
	assert.Equal(t, 1, countEntries(hook, logrus.WarnLevel, whatsmeow.ErrNoPushName.Error()))
}

func TestPresenceKeeperAvailableFailureStillSendsUnavailable(t *testing.T) {
	client := newFakePresenceClient(true)
	client.errs[types.PresenceAvailable] = errSendFailed
	log, _ := newPresenceTestLogger()
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
	log, hook := newPresenceTestLogger()
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
	log, hook := newPresenceTestLogger()
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
		log, _ := newPresenceTestLogger()
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
	log, _ := newPresenceTestLogger()
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
	log, _ := newPresenceTestLogger()
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
	log, _ := newPresenceTestLogger()
	p := newPresenceKeeper(client, log, "unavailable", time.Second, 30*time.Millisecond)

	p.start()
	defer p.stop()

	// No login event: only the ticker can cause these.
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
}

func TestPresenceKeeperStopWithoutStart(t *testing.T) {
	log, _ := newPresenceTestLogger()
	p := newPresenceKeeper(newFakePresenceClient(true), log, "blip", time.Second, 0)

	require.NotNil(t, p)
	assert.NotPanics(t, p.stop)
}

// availableBlip is short so that available mode wrongly taking the blip path
// sends "unavailable" well within the tests' waits.
const availableBlip = 10 * time.Millisecond

var errSendFailed = errors.New("send failed")

func TestPresenceKeeperAvailableModeAccepted(t *testing.T) {
	log, _ := newPresenceTestLogger()
	p := newPresenceKeeper(newFakePresenceClient(true), log, "available", availableBlip, 0)

	require.NotNil(t, p)
	assert.Equal(t, "available", p.mode)
}

func TestPresenceKeeperAvailableOnLogin(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newPresenceTestLogger()
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
	log, hook := newPresenceTestLogger()
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
		intervalTip int
	}{
		{mode: "available", interval: 0, intervalTip: 1},
		{mode: "available", interval: time.Hour, intervalTip: 0},
		{mode: "blip", interval: 0, intervalTip: 0},
	}

	for _, tc := range cases {
		log, hook := newPresenceTestLogger()
		p := newPresenceKeeper(newFakePresenceClient(true), log, tc.mode, availableBlip, tc.interval)

		p.start()
		p.stop()

		assert.Equal(t, tc.intervalTip, countEntries(hook, logrus.WarnLevel, "set PresenceIntervalHours"),
			"mode %s, interval %s", tc.mode, tc.interval)
		assert.Equal(t, tc.intervalTip, countEntries(hook, logrus.WarnLevel, ""),
			"no other warning: mode %s, interval %s", tc.mode, tc.interval)
	}
}

func TestPresenceKeeperAvailableStopSendsNothing(t *testing.T) {
	client := newFakePresenceClient(true)
	log, hook := newPresenceTestLogger()
	p := newPresenceKeeper(client, log, "available", time.Hour, 0)

	p.start()

	p.handleEvent(&events.Connected{})
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	p.stop()

	assert.Equal(t, []types.Presence{types.PresenceAvailable}, client.sentStates())
	assert.Zero(t, countEntries(hook, logrus.WarnLevel, "did not stop in time"))
}

// activeIdle is long against the scheduling jitter of -race runs, so that a
// timer that should not fire yet does not.
const activeIdle = 300 * time.Millisecond

func newActiveKeeper(t *testing.T, client *fakePresenceClient) (*presenceKeeper, *logrustest.Hook) {
	t.Helper()

	log, hook := newPresenceTestLogger()
	log.Logger.SetLevel(logrus.DebugLevel)
	p := newPresenceKeeper(client, log, "active", time.Second, 0)
	require.NotNil(t, p)
	p.setActive(activeIdle)

	return p, hook
}

func (f *fakePresenceClient) timesSent() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]time.Time(nil), f.at...)
}

func waitForEntry(t *testing.T, hook *logrustest.Hook, level logrus.Level, substr string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for countEntries(hook, level, substr) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log entry %q", substr)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func TestPresenceKeeperActiveConfig(t *testing.T) {
	cases := []struct {
		minutes string
		want    time.Duration
	}{
		{"7", 7 * time.Minute},
		{"0", defaultPresenceActive},
		{"-1", defaultPresenceActive},
	}

	for _, tc := range cases {
		log, hook := newPresenceTestLogger()
		input := "[whatsapp.test]\nPresenceMode=\"active\"\nPresenceIntervalHours=3\nPresenceActiveMinutes=" + tc.minutes + "\n"
		b := &Bwhatsapp{Config: &bridge.Config{Bridge: &bridge.Bridge{
			Account: "whatsapp.test",
			Config:  config.NewConfigFromString(log.Logger, []byte(input)),
			Log:     log,
		}}}

		p := b.newPresenceKeeper(newFakePresenceClient(true))

		require.NotNil(t, p, tc.minutes)
		assert.Equal(t, tc.want, p.active, tc.minutes)
		assert.Equal(t, time.Duration(0), p.interval, "the interval is ignored in active mode")
		assert.Equal(t, 1, countEntries(hook, logrus.InfoLevel, "PresenceIntervalHours is ignored"), tc.minutes)
	}
}

func TestPresenceKeeperActiveGoesOnlineThenIdles(t *testing.T) {
	client := newFakePresenceClient(true)
	p, _ := newActiveKeeper(t, client)

	p.start()
	defer p.stop()

	p.markActive()

	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))

	at := client.timesSent()
	assert.GreaterOrEqual(t, at[1].Sub(at[0]), activeIdle, "online for the idle time")
}

func TestPresenceKeeperActiveExtends(t *testing.T) {
	client := newFakePresenceClient(true)
	p, _ := newActiveKeeper(t, client)

	p.start()
	defer p.stop()

	p.markActive()
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	var last time.Time

	for range 5 {
		time.Sleep(30 * time.Millisecond)

		last = time.Now()

		p.markActive()
	}

	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))

	at := client.timesSent()
	assert.GreaterOrEqual(t, at[1].Sub(last), activeIdle, "the idle time runs from the last send")
	client.expectNoCall(t, 100*time.Millisecond)
}

func TestPresenceKeeperActiveLoginWhileOffline(t *testing.T) {
	client := newFakePresenceClient(true)
	p, _ := newActiveKeeper(t, client)

	p.start()
	defer p.stop()

	p.handleEvent(&events.Connected{})

	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
	client.expectNoCall(t, 100*time.Millisecond)
}

func TestPresenceKeeperActiveLoginWhileOnline(t *testing.T) {
	client := newFakePresenceClient(true)
	p, _ := newActiveKeeper(t, client)

	p.start()
	defer p.stop()

	p.markActive()
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	p.handleEvent(&events.Connected{})

	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
}

func TestPresenceKeeperActiveFailedAvailable(t *testing.T) {
	client := newFakePresenceClient(true)
	client.setErr(types.PresenceAvailable, errSendFailed)
	p, _ := newActiveKeeper(t, client)

	p.start()
	defer p.stop()

	p.markActive()

	// "unavailable" follows straight away, not after the idle time.
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))

	// Offline again, so the next send retries.
	p.markActive()
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))
}

func TestPresenceKeeperActiveNoPushName(t *testing.T) {
	client := newFakePresenceClient(true)
	client.pushName = false
	p, hook := newActiveKeeper(t, client)
	ctx := context.Background()

	p.onActivity(ctx)
	p.onActivity(ctx)

	// Each attempt warns once and nothing follows it; the bridge stays
	// offline, so the second send tries again.
	assert.Empty(t, client.sentStates())
	assert.Equal(t, 2, countEntries(hook, logrus.WarnLevel, whatsmeow.ErrNoPushName.Error()))
	assert.False(t, p.online)
}

func TestPresenceKeeperActiveRefreshFailure(t *testing.T) {
	client := newFakePresenceClient(true)
	p, _ := newActiveKeeper(t, client)

	p.start()
	defer p.stop()

	p.markActive()
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	client.setErr(types.PresenceAvailable, errSendFailed)
	p.handleEvent(&events.Connected{})

	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))

	// Offline now: the old deadline must not send a second "unavailable".
	client.expectNoCall(t, activeIdle+100*time.Millisecond)
}

func TestPresenceKeeperActiveStopWhileOnline(t *testing.T) {
	client := newFakePresenceClient(true)
	p, hook := newActiveKeeper(t, client)

	p.start()

	p.markActive()
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	p.stop()

	assert.Equal(t, []types.Presence{types.PresenceAvailable, types.PresenceUnavailable}, client.sentStates())
	assert.Zero(t, countEntries(hook, logrus.WarnLevel, "did not stop in time"))
}

func TestPresenceKeeperActiveStopDuringAvailableSend(t *testing.T) {
	client := newFakePresenceClient(true)
	client.block = make(chan struct{})
	client.blockState = types.PresenceAvailable
	p, hook := newActiveKeeper(t, client)

	p.start()

	p.markActive()
	client.waitBlocked(t)

	p.stop()

	assert.Equal(t, []types.Presence{types.PresenceUnavailable}, client.sentStates())
	assert.Zero(t, countEntries(hook, logrus.WarnLevel, "did not stop in time"))
}

func TestPresenceKeeperActiveSkipsWhenDisconnected(t *testing.T) {
	client := newFakePresenceClient(true)
	client.connected.Store(false)
	p, hook := newActiveKeeper(t, client)

	p.onActivity(context.Background())

	assert.Empty(t, client.sentStates())
	assert.False(t, p.online)
	assert.Equal(t, 1, countEntries(hook, logrus.DebugLevel, "Skipping presence update (activity)"))
}

func TestPresenceKeeperActiveIdleWhileDisconnected(t *testing.T) {
	client := newFakePresenceClient(true)
	p, hook := newActiveKeeper(t, client)

	p.start()
	defer p.stop()

	p.markActive()
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	client.connected.Store(false)
	waitForEntry(t, hook, logrus.DebugLevel, "Skipping presence update (idle)")

	client.connected.Store(true)
	p.handleEvent(&events.Connected{})

	// The deadline passed while disconnected, so the bridge is offline and
	// the login sends "unavailable", not "available".
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))
	client.expectNoCall(t, 100*time.Millisecond)
}

func TestPresenceKeeperActiveTimerYieldsToPendingSend(t *testing.T) {
	client := newFakePresenceClient(true)
	p, _ := newActiveKeeper(t, client)

	p.online = true
	p.until = time.Now()
	p.armTimer()

	defer p.clear()

	before := p.until
	p.activity <- struct{}{}

	p.onTimer(context.Background())

	assert.Empty(t, client.sentStates())
	assert.True(t, p.online)
	assert.True(t, p.until.After(before), "the deadline moves out")
}

func TestPresenceKeeperBlipIgnoresActivity(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newPresenceTestLogger()
	p := newPresenceKeeper(client, log, "blip", time.Hour, 0)

	p.markActive()
	p.onActivity(context.Background())

	assert.Empty(t, client.sentStates())
	assert.False(t, p.online)
	assert.Empty(t, p.activity, "markActive does not queue outside active mode")
}

func TestPresenceKeeperBlipTriggerWhileOnline(t *testing.T) {
	client := newFakePresenceClient(true)
	log, _ := newPresenceTestLogger()
	p := newPresenceKeeper(client, log, "blip", time.Hour, 0)

	p.online = true
	p.until = time.Now().Add(300 * time.Millisecond)
	until := p.until

	p.onTrigger(context.Background())

	// "available" again, and the deadline is unchanged.
	assert.Equal(t, []types.Presence{types.PresenceAvailable}, client.sentStates())
	assert.True(t, p.online)
	assert.Equal(t, until, p.until)
}

func TestPresenceKeeperBlipLoginDuringBlip(t *testing.T) {
	const blip = 300 * time.Millisecond

	client := newFakePresenceClient(true)
	log, _ := newPresenceTestLogger()
	p := newPresenceKeeper(client, log, "blip", blip, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.Connected{})
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	// Late in the blip, so that extending it would be measurable.
	time.Sleep(2 * blip / 3)
	p.handleEvent(&events.PushNameSetting{})
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	// One "unavailable" at the original deadline, and no second blip.
	assert.Equal(t, types.PresenceUnavailable, client.waitCall(t))

	at := client.timesSent()
	gap := at[2].Sub(at[0])
	assert.GreaterOrEqual(t, gap, blip)
	assert.Less(t, gap, blip+blip/2, "the blip is not extended")
	client.expectNoCall(t, blip+100*time.Millisecond)
}

func TestPresenceKeeperBlipEndsWhileDisconnected(t *testing.T) {
	client := newFakePresenceClient(true)
	log, hook := newPresenceTestLogger()
	log.Logger.SetLevel(logrus.DebugLevel)
	p := newPresenceKeeper(client, log, "blip", 50*time.Millisecond, 0)

	p.start()
	defer p.stop()

	p.handleEvent(&events.Connected{})
	assert.Equal(t, types.PresenceAvailable, client.waitCall(t))

	client.connected.Store(false)
	waitForEntry(t, hook, logrus.DebugLevel, "Skipping presence update (idle)")

	assert.Equal(t, []types.Presence{types.PresenceAvailable}, client.sentStates())
	assert.Zero(t, countEntries(hook, logrus.WarnLevel, ""), "no failed send to warn about")
}

func TestNoteSentMarksActivity(t *testing.T) {
	client := newFakePresenceClient(true)
	p, _ := newActiveKeeper(t, client)
	b := &Bwhatsapp{presence: p}

	b.noteSent(errSendFailed)
	assert.Empty(t, p.activity, "a failed send is not activity")

	b.noteSent(nil)
	assert.Len(t, p.activity, 1)

	assert.NotPanics(t, func() { (&Bwhatsapp{}).noteSent(nil) }, "presence off")
}
