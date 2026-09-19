package bwhatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// Identities taken from a live capture: the bridge account is known to its
// contacts both by phone number and by LID.
var (
	quoteOwnID   = types.NewADJID("48111222333", 0, 20)
	quoteOwnLID  = types.NewJID("100000000000001", types.HiddenUserServer)
	quoteContact = types.NewJID("200000000000002", types.HiddenUserServer)
	quoteStanza  = "3EB0963FDD4E47AC5A00"
)

func quoteOf(participant string) *waE2E.ContextInfo {
	return &waE2E.ContextInfo{StanzaID: new(quoteStanza), Participant: new(participant)}
}

func TestQuotedMessageID(t *testing.T) {
	tests := []struct {
		name   string
		ci     *waE2E.ContextInfo
		ownID  types.JID
		ownLID types.JID
		want   string
	}{
		{"own LID participant is keyed under the phone number", quoteOf(quoteOwnLID.String()), quoteOwnID, quoteOwnLID, "48111222333@s.whatsapp.net/" + quoteStanza},
		{"own phone participant is unchanged", quoteOf("48111222333@s.whatsapp.net"), quoteOwnID, quoteOwnLID, "48111222333@s.whatsapp.net/" + quoteStanza},
		{"contact LID participant is unchanged", quoteOf(quoteContact.String()), quoteOwnID, quoteOwnLID, quoteContact.String() + "/" + quoteStanza},
		{"contact phone participant is unchanged", quoteOf("48222333444@s.whatsapp.net"), quoteOwnID, quoteOwnLID, "48222333444@s.whatsapp.net/" + quoteStanza},
		{"unknown own LID never matches", quoteOf(quoteOwnLID.String()), quoteOwnID, types.EmptyJID, quoteOwnLID.String() + "/" + quoteStanza},
		{"unknown own ID leaves the LID in place", quoteOf(quoteOwnLID.String()), types.EmptyJID, quoteOwnLID, quoteOwnLID.String() + "/" + quoteStanza},
		{"nil context", nil, quoteOwnID, quoteOwnLID, ""},
		{"no stanza ID", &waE2E.ContextInfo{Participant: new(quoteContact.String())}, quoteOwnID, quoteOwnLID, ""},
		{"no participant", &waE2E.ContextInfo{StanzaID: new(quoteStanza)}, quoteOwnID, quoteOwnLID, ""},
		{"unparsable participant", quoteOf("123:notanumber@s.whatsapp.net"), quoteOwnID, quoteOwnLID, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quotedMessageID(tt.ci, tt.ownID, tt.ownLID); got != tt.want {
				t.Errorf("quotedMessageID() = %q, want %q", got, tt.want)
			}
		})
	}
}
