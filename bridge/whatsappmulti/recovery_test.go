package bwhatsapp

import (
	"sync"
	"testing"
	"time"

	"github.com/matterbridge-org/matterbridge/bridge"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	callConnect    = "Connect"
	callDisconnect = "Disconnect"
	causeOutdated  = "client outdated"
	causeFailure   = "connect failure"
)

// unknownConnectFailure is a ConnectFailure as whatsmeow dispatches it: only
// for reasons it does not retry itself (connectionevents.go at 19d61352436f).
func unknownConnectFailure() recoveryStep {
	return recoveryStep{evt: &events.ConnectFailure{Reason: events.ConnectFailureReason(499), Message: "unknown"}}
}

func repeatStep(step recoveryStep, n int) []recoveryStep {
	steps := make([]recoveryStep, n)
	for i := range steps {
		steps[i] = step
	}

	return steps
}

// recoveryStep scripts one Connect: err is returned by Connect; otherwise evt
// (if any) is delivered through eventHandler, as whatsmeow dispatches it after
// the handshake (connectionevents.go at 19d61352436f). Neither: no event.
type recoveryStep struct {
	err error
	evt any
}

// fakeRecoveryClient stands in for the Connect and Disconnect of
// *whatsmeow.Client (client.go at 19d61352436f).
type fakeRecoveryClient struct {
	t       *testing.T
	b       *Bwhatsapp
	entered chan struct{} // optional: receives on each Connect
	release chan struct{} // optional: Connect blocks until it is closed

	mu    sync.Mutex
	steps []recoveryStep
	calls []string
}

func (f *fakeRecoveryClient) Connect() error {
	f.mu.Lock()
	f.calls = append(f.calls, callConnect)

	// An exhausted script fails the test and ends the loop instead of
	// letting it retry forever with the sleep stubbed.
	step := recoveryStep{evt: &events.LoggedOut{Reason: events.ConnectFailureLoggedOut}}
	if len(f.steps) > 0 {
		step, f.steps = f.steps[0], f.steps[1:]
	} else {
		f.t.Error("recovery script exhausted")
	}
	f.mu.Unlock()

	if f.entered != nil {
		f.entered <- struct{}{}
	}

	if f.release != nil {
		<-f.release
	}

	if step.err != nil {
		return step.err
	}

	if step.evt != nil {
		f.b.eventHandler(step.evt)
	}

	return nil
}

func (f *fakeRecoveryClient) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, callDisconnect)
}

func (f *fakeRecoveryClient) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func (f *fakeRecoveryClient) connects() int {
	n := 0

	for _, c := range f.callLog() {
		if c == callConnect {
			n++
		}
	}

	return n
}

type recordedSleep struct {
	d        time.Duration
	loginNil bool
}

// newRecoveryBridge builds a bridge with a test logger, a recording sleep and a
// version fetch that never changes the version.
func newRecoveryBridge(t *testing.T, loginWait time.Duration, steps ...recoveryStep) (*Bwhatsapp, *fakeRecoveryClient, *logrustest.Hook, *[]recordedSleep) {
	t.Helper()

	stubFetch(t, returnVersion(store.GetWAVersion()))

	log, hook := newTestLogger()
	b := &Bwhatsapp{Config: &bridge.Config{Bridge: &bridge.Bridge{Log: log}}}
	b.recoveryLoginWait = loginWait

	var (
		mu     sync.Mutex
		sleeps []recordedSleep
	)

	b.sleepFn = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()

		sleeps = append(sleeps, recordedSleep{d: d, loginNil: b.loginCh.Load() == nil})
	}

	fake := &fakeRecoveryClient{t: t, b: b, steps: steps}

	return b, fake, hook, &sleeps
}

func TestRecoveryRetriesOutdatedThenReconnects(t *testing.T) {
	b, fake, hook, sleeps := newRecoveryBridge(t, time.Second,
		recoveryStep{evt: &events.ClientOutdated{}},
		recoveryStep{evt: &events.Connected{}},
	)

	b.runRecovery(fake, causeOutdated)

	// Each round disconnects first; the failed round also disconnects after
	// its login wait.
	assert.Equal(t, []string{callDisconnect, callConnect, callDisconnect, callDisconnect, callConnect}, fake.callLog())
	assert.Equal(t, 1, countEntries(hook, logrus.ErrorLevel, "still rejects client version"))
	assert.Equal(t, 1, countEntries(hook, logrus.InfoLevel, "reconnected after client outdated"))

	require.Len(t, *sleeps, 1)
	assert.Equal(t, recoveryRetryInterval, (*sleeps)[0].d, "default retry interval")
	assert.True(t, (*sleeps)[0].loginNil, "no login wait may stay armed between rounds")
}

func TestRecoveryRetriesConnectError(t *testing.T) {
	b, fake, hook, sleeps := newRecoveryBridge(t, time.Second,
		recoveryStep{err: errTestBoom},
		recoveryStep{evt: &events.Connected{}},
	)

	b.runRecovery(fake, causeOutdated)

	// A failed Connect is followed by a Disconnect before the next round's.
	assert.Equal(t, []string{callDisconnect, callConnect, callDisconnect, callDisconnect, callConnect}, fake.callLog())
	assert.Equal(t, 1, countEntries(hook, logrus.ErrorLevel, "failed (boom)"))
	assert.Len(t, *sleeps, 1)
}

func TestRecoveryRetriesLoginTimeout(t *testing.T) {
	b, fake, hook, sleeps := newRecoveryBridge(t, 20*time.Millisecond,
		recoveryStep{},
		recoveryStep{evt: &events.Connected{}},
	)

	b.runRecovery(fake, causeOutdated)

	assert.Equal(t, 2, fake.connects())
	assert.Equal(t, 1, countEntries(hook, logrus.ErrorLevel, "timed out waiting for WhatsApp login"))
	assert.Len(t, *sleeps, 1)
}

func TestRecoveryStopsOnTerminalEvents(t *testing.T) {
	cases := []struct {
		name string
		evt  any
	}{
		{"LoggedOut", &events.LoggedOut{Reason: events.ConnectFailureLoggedOut}},
		{"StreamReplaced", &events.StreamReplaced{}},
		{"TemporaryBan", &events.TemporaryBan{Code: events.TempBanSentToTooManyPeople}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, fake, hook, sleeps := newRecoveryBridge(t, time.Second,
				recoveryStep{evt: tc.evt},
				recoveryStep{evt: &events.Connected{}},
			)

			b.runRecovery(fake, causeOutdated)

			assert.Equal(t, 1, fake.connects())
			assert.Equal(t, 1, countEntries(hook, logrus.ErrorLevel, "stopped"))
			assert.Empty(t, *sleeps)
		})
	}
}

func TestRecoveryRetriesConnectFailure(t *testing.T) {
	b, fake, hook, sleeps := newRecoveryBridge(t, time.Second,
		unknownConnectFailure(),
		recoveryStep{evt: &events.Connected{}},
	)

	b.runRecovery(fake, causeFailure)

	assert.Equal(t, 2, fake.connects())
	assert.Equal(t, 1, countEntries(hook, logrus.ErrorLevel, "reconnect after connect failure failed"))
	assert.Equal(t, 1, countEntries(hook, logrus.InfoLevel, "reconnected after connect failure"))
	assert.Len(t, *sleeps, 1)
}

func TestRecoveryGivesUpAfterSixConnectFailures(t *testing.T) {
	b, fake, hook, sleeps := newRecoveryBridge(t, time.Second, repeatStep(unknownConnectFailure(), 7)...)

	b.runRecovery(fake, causeFailure)

	assert.Equal(t, 6, fake.connects())
	assert.Len(t, *sleeps, 5)
	assert.Equal(t, 1, countEntries(hook, logrus.ErrorLevel, "gave up after 6 connect failures"))
	assert.Equal(t, 0, countEntries(hook, logrus.InfoLevel, "reconnected"))
}

func TestRecoveryCapCountsOnlyConnectFailures(t *testing.T) {
	steps := repeatStep(unknownConnectFailure(), 5)
	steps = append(steps, recoveryStep{evt: &events.ClientOutdated{}}, recoveryStep{})
	steps = append(steps, repeatStep(unknownConnectFailure(), 2)...)

	b, fake, hook, _ := newRecoveryBridge(t, 20*time.Millisecond, steps...)

	b.runRecovery(fake, causeFailure)

	// The 405 and the login timeout neither advance nor reset the count.
	assert.Equal(t, 8, fake.connects())
	assert.Equal(t, 1, countEntries(hook, logrus.ErrorLevel, "gave up after 6 connect failures"))
}

func TestRecoveryDoesNotCapClientOutdated(t *testing.T) {
	steps := repeatStep(recoveryStep{evt: &events.ClientOutdated{}}, 7)
	steps = append(steps, recoveryStep{evt: &events.Connected{}})

	b, fake, hook, sleeps := newRecoveryBridge(t, time.Second, steps...)

	b.runRecovery(fake, causeOutdated)

	assert.Equal(t, 8, fake.connects())
	assert.Len(t, *sleeps, 7)
	assert.Equal(t, 1, countEntries(hook, logrus.InfoLevel, "reconnected after client outdated"))
}

func TestStartRecoveryRunsOneLoop(t *testing.T) {
	b, fake, _, _ := newRecoveryBridge(t, time.Second, recoveryStep{evt: &events.Connected{}})
	fake.entered = make(chan struct{}, 2)
	fake.release = make(chan struct{})

	b.startRecovery(fake, causeOutdated)
	<-fake.entered

	b.startRecovery(fake, causeOutdated)

	select {
	case <-fake.entered:
		t.Fatal("a second recovery loop started")
	case <-time.After(200 * time.Millisecond):
	}

	close(fake.release)
	require.Eventually(t, func() bool { return !b.recovering.Load() }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, fake.connects())
}

func TestClientOutdatedDuringLoginWaitDoesNotRecover(t *testing.T) {
	b, fake, _, _ := newRecoveryBridge(t, time.Second, recoveryStep{evt: &events.Connected{}})
	b.testRecoveryClient = fake

	ch, disarm := b.armLogin()
	defer disarm()

	b.eventHandler(&events.ClientOutdated{})

	require.ErrorIs(t, <-ch, errClientOutdated)

	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, fake.callLog(), "the 405 belongs to the armed wait")
}

func TestConnectFailureDuringLoginWaitDoesNotRecover(t *testing.T) {
	b, fake, _, _ := newRecoveryBridge(t, time.Second, recoveryStep{evt: &events.Connected{}})
	b.testRecoveryClient = fake

	ch, disarm := b.armLogin()
	defer disarm()

	b.eventHandler(unknownConnectFailure().evt)

	require.ErrorIs(t, <-ch, errConnectFailure)

	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, fake.callLog(), "the connect failure belongs to the armed wait")
}

func TestConnectFailureWhileRunningStartsRecovery(t *testing.T) {
	b, fake, hook, _ := newRecoveryBridge(t, time.Second, recoveryStep{evt: &events.Connected{}})
	b.testRecoveryClient = fake
	fake.entered = make(chan struct{}, 1)

	b.eventHandler(unknownConnectFailure().evt)

	select {
	case <-fake.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no recovery started")
	}

	require.Eventually(t, func() bool { return !b.recovering.Load() }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, countEntries(hook, logrus.InfoLevel, "reconnected after connect failure"))
}

func TestClientOutdatedWhileRunningStartsRecovery(t *testing.T) {
	b, fake, _, _ := newRecoveryBridge(t, time.Second, recoveryStep{evt: &events.Connected{}})
	b.testRecoveryClient = fake
	fake.entered = make(chan struct{}, 1)

	b.eventHandler(&events.ClientOutdated{})

	select {
	case <-fake.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no recovery started")
	}

	require.Eventually(t, func() bool { return !b.recovering.Load() }, 2*time.Second, 5*time.Millisecond)
}
