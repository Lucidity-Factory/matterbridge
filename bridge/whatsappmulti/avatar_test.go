package bwhatsapp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.mau.fi/whatsmeow"
)

func TestIsMissingAvatar(t *testing.T) {
	// The chain GetProfilePicThumb returns: its %w wrap (helpers.go) around
	// whatsmeow's IQ error, which matches the sentinel through
	// wrappedIQError.Is (errors.go at 19d61352436f). The inner %w stands in
	// for that unexported wrapper.
	wrap := func(err error) error { return fmt.Errorf("failed to get avatar: %w", fmt.Errorf("iq: %w", err)) }

	assert.True(t, isMissingAvatar(wrap(whatsmeow.ErrProfilePictureUnauthorized)), "hidden")
	assert.True(t, isMissingAvatar(wrap(whatsmeow.ErrProfilePictureNotSet)), "not set")
	assert.False(t, isMissingAvatar(wrap(whatsmeow.ErrIQRateOverLimit)), "a real failure stays a warning")
	assert.False(t, isMissingAvatar(nil))
}
