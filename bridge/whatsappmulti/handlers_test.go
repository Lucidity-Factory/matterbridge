package bwhatsapp

import (
	"io"
	"testing"
	"time"

	"github.com/matterbridge-org/matterbridge/bridge"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.mau.fi/whatsmeow/types/events"
)

func newTestLogger() (*logrus.Entry, *logrustest.Hook) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	hook := logrustest.NewLocal(logger)

	return logrus.NewEntry(logger), hook
}

func TestEventHandlerLogsLifecycle(t *testing.T) {
	cases := []struct {
		evt     any
		level   logrus.Level
		message string
	}{
		{&events.Connected{}, logrus.InfoLevel, "login successful"},
		{&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, logrus.WarnLevel, "logged out (reason: 401: logged out from another device, on connect: true)"},
		{&events.StreamReplaced{}, logrus.WarnLevel, "stream replaced"},
		{&events.Disconnected{}, logrus.InfoLevel, "disconnected"},
		{&events.KeepAliveTimeout{ErrorCount: 3, LastSuccess: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, logrus.WarnLevel, "keepalive timeout (errors: 3, last success: 2026-01-02T03:04:05Z)"},
		{&events.KeepAliveRestored{}, logrus.InfoLevel, "keepalive restored"},
	}

	for _, tc := range cases {
		log, hook := newTestLogger()
		b := &Bwhatsapp{Config: &bridge.Config{Bridge: &bridge.Bridge{Log: log}}}

		b.eventHandler(tc.evt)

		entry := hook.LastEntry()
		require.NotNil(t, entry, "%T", tc.evt)
		assert.Equal(t, tc.level, entry.Level, "%T", tc.evt)
		assert.Contains(t, entry.Message, tc.message, "%T", tc.evt)
	}
}
