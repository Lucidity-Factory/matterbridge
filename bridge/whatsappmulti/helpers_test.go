package bwhatsapp

import (
	"errors"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

var (
	errLookupFailed = errors.New("lookup failed")

	testPhone = types.NewJID("48222333444", types.DefaultUserServer)
	testLID   = types.NewJID("200000000000002", types.HiddenUserServer)
	testGroup = types.NewJID("48111222333-123455678999", types.GroupServer)
	// testWrong is returned by unexpectedLookup: a case that must not consult
	// the lookup fails when this value shows up in the result.
	testWrong = types.NewJID("1", types.DefaultUserServer)

	unexpectedLookup = func(types.JID) (types.JID, error) { return testWrong, nil }
	noMappingLookup  = func(types.JID) (types.JID, error) { return types.EmptyJID, nil }
	mappedLookup     = func(types.JID) (types.JID, error) { return testPhone, nil }
	failingLookup    = func(types.JID) (types.JID, error) { return types.EmptyJID, errLookupFailed }
)

func directMessage(chat, sender, senderAlt types.JID) types.MessageInfo {
	return types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: sender, SenderAlt: senderAlt}}
}

func TestResolveChatJID(t *testing.T) {
	groupInfo := directMessage(testGroup, testLID, testPhone)
	groupInfo.IsGroup = true

	tests := []struct {
		name   string
		info   types.MessageInfo
		lookup func(types.JID) (types.JID, error)
		want   types.JID
	}{
		{"group chat is unchanged", groupInfo, unexpectedLookup, testGroup},
		{"phone chat is unchanged", directMessage(testPhone, testPhone, testLID), unexpectedLookup, testPhone},
		{"LID chat with phone SenderAlt uses SenderAlt without device", directMessage(testLID, testLID, types.NewADJID("48222333444", 0, 13)), unexpectedLookup, testPhone},
		{"LID chat without SenderAlt resolves through the store", directMessage(testLID, testLID, types.EmptyJID), mappedLookup, testPhone},
		{"LID chat with no mapping stays a LID", directMessage(testLID, testLID, types.EmptyJID), noMappingLookup, testLID},
		{"LID chat with failing lookup stays a LID", directMessage(testLID, testLID, types.EmptyJID), failingLookup, testLID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveChatJID(tt.info, tt.lookup)
			if got != tt.want {
				t.Errorf("resolveChatJID() = %s, want %s", got, tt.want)
			}
		})
	}
}

type fakeKey struct{ participant string }

func (k fakeKey) GetParticipant() string { return k.participant }

func TestDeletedMessageSender(t *testing.T) {
	revoker := types.NewADJID("200000000000002", 0, 13)
	admin := types.NewJID("48333444555", types.DefaultUserServer)

	tests := []struct {
		name    string
		key     fakeKey
		revoker types.JID
		want    types.JID
	}{
		{"group revoke of own message names the participant", fakeKey{testLID.String()}, revoker, testLID},
		{"group revoke by an admin names the original sender", fakeKey{testLID.String()}, admin, testLID},
		{"1:1 revoke has no participant and falls back to the revoker", fakeKey{""}, revoker, revoker},
		{"unparsable participant falls back to the revoker", fakeKey{"123:notanumber@s.whatsapp.net"}, revoker, revoker},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deletedMessageSender(tt.key, tt.revoker); got != tt.want {
				t.Errorf("deletedMessageSender() = %s, want %s", got, tt.want)
			}
		})
	}
}
