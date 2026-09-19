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
