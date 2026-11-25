package bwhatsapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.mau.fi/whatsmeow/store"
)

var errTestFetch = errors.New("unexpected response with status 403: " + strings.Repeat("<html>", 2000))

// stubFetch replaces whatsmeow.GetLatestVersion (update.go at 19d61352436f:
// func(ctx, *http.Client) (*store.WAVersionContainer, error)) and restores it
// and the process-wide version afterwards.
func stubFetch(t *testing.T, fetch func(context.Context, *http.Client) (*store.WAVersionContainer, error)) {
	t.Helper()

	origFetch, origVersion, origTimeout := fetchLatestWAVersion, store.GetWAVersion(), waVersionFetchTimeout
	fetchLatestWAVersion = fetch

	t.Cleanup(func() {
		fetchLatestWAVersion = origFetch
		waVersionFetchTimeout = origTimeout

		store.SetWAVersion(origVersion)
	})
}

func returnVersion(v store.WAVersionContainer) func(context.Context, *http.Client) (*store.WAVersionContainer, error) {
	return func(context.Context, *http.Client) (*store.WAVersionContainer, error) {
		return &v, nil
	}
}

func TestRefreshWAVersionUsesNewer(t *testing.T) {
	newer := store.GetWAVersion()
	newer[2]++

	stubFetch(t, returnVersion(newer))

	got, err := refreshWAVersion(context.Background())
	require.NoError(t, err)

	assert.Equal(t, newer, got)
	assert.Equal(t, newer, store.GetWAVersion())
}

func TestRefreshWAVersionKeepsCurrentOverOlder(t *testing.T) {
	current := store.GetWAVersion()
	older := current
	older[2]--

	stubFetch(t, returnVersion(older))

	got, err := refreshWAVersion(context.Background())
	require.NoError(t, err)

	assert.Equal(t, current, got)
	assert.Equal(t, current, store.GetWAVersion())
}

func TestRefreshWAVersionFetchError(t *testing.T) {
	current := store.GetWAVersion()

	stubFetch(t, func(context.Context, *http.Client) (*store.WAVersionContainer, error) {
		return nil, errTestFetch
	})

	got, err := refreshWAVersion(context.Background())

	require.ErrorIs(t, err, errFetchWAVersion)
	assert.LessOrEqual(t, len(err.Error()), 232, "the page body must be truncated")
	assert.Equal(t, current, got)
	assert.Equal(t, current, store.GetWAVersion())
}

func TestRefreshWAVersionNoVersion(t *testing.T) {
	stubFetch(t, func(context.Context, *http.Client) (*store.WAVersionContainer, error) {
		return nil, nil //nolint:nilnil // the case under test
	})

	_, err := refreshWAVersion(context.Background())

	require.ErrorIs(t, err, errFetchWAVersion)
	assert.Contains(t, err.Error(), errNoWAVersion.Error())
}

func TestRefreshWAVersionTimesOut(t *testing.T) {
	release := make(chan struct{})

	stubFetch(t, func(ctx context.Context, _ *http.Client) (*store.WAVersionContainer, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errTestFetch
		}
	})
	t.Cleanup(func() { close(release) })

	waVersionFetchTimeout = 50 * time.Millisecond

	done := make(chan error, 1)

	go func() {
		_, err := refreshWAVersion(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, errFetchWAVersion)
		assert.Contains(t, err.Error(), context.DeadlineExceeded.Error())
	case <-time.After(2 * time.Second):
		t.Fatal("refreshWAVersion did not time out")
	}
}

func TestArmLoginDisarmClears(t *testing.T) {
	b := &Bwhatsapp{}

	_, disarm := b.armLogin()
	require.NotNil(t, b.loginCh.Load())

	disarm()

	assert.Nil(t, b.loginCh.Load())
}

func TestArmLoginDisarmLeavesLaterWait(t *testing.T) {
	b := &Bwhatsapp{}

	_, disarmFirst := b.armLogin()
	second, _ := b.armLogin()

	disarmFirst()

	got := b.loginCh.Load()
	require.NotNil(t, got, "a stale disarm must not clear a later wait")

	signalLogin(*got, errTestSecond)
	assert.ErrorIs(t, <-second, errTestSecond)
}
