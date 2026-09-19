package bwhatsapp

import (
	"context"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
)

type ProfilePicInfo struct {
	URL    string `json:"eurl"`
	Tag    string `json:"tag"`
	Status int16  `json:"status"`
}

func (b *Bwhatsapp) reloadContacts() {
	if _, err := b.wc.Store.Contacts.GetAllContacts(context.Background()); err != nil {
		b.Log.Errorf("error on update of contacts: %v", err)
	}

	allcontacts, err := b.wc.Store.Contacts.GetAllContacts(context.Background())
	if err != nil {
		b.Log.Errorf("error on update of contacts: %v", err)
	}

	if len(allcontacts) > 0 {
		b.contacts = allcontacts
	}
}

func (b *Bwhatsapp) getSenderName(info types.MessageInfo) string {
	// Parse AD JID
	var senderJid types.JID
	senderJid.User, senderJid.Server = info.Sender.User, info.Sender.Server

	sender, exists := b.contacts[senderJid]

	if !exists || (sender.FullName == "" && sender.FirstName == "") {
		b.reloadContacts() // Contacts may need to be reloaded
		sender, exists = b.contacts[senderJid]
	}

	if exists && sender.FullName != "" {
		return sender.FullName
	}

	if info.PushName != "" {
		return info.PushName
	}

	if exists && sender.FirstName != "" {
		return sender.FirstName
	}

	return "Someone"
}

func (b *Bwhatsapp) getSenderNameFromJID(senderJid types.JID) string {
	sender, exists := b.contacts[senderJid]

	if !exists || (sender.FullName == "" && sender.FirstName == "") {
		b.reloadContacts() // Contacts may need to be reloaded
		sender, exists = b.contacts[senderJid]
	}

	if exists && sender.FullName != "" {
		return sender.FullName
	}

	if exists && sender.FirstName != "" {
		return sender.FirstName
	}

	if sender.PushName != "" {
		return sender.PushName
	}

	return "Someone"
}

func (b *Bwhatsapp) getSenderNotify(senderJid types.JID) string {
	sender, exists := b.contacts[senderJid]

	if !exists || (sender.FullName == "" && sender.PushName == "" && sender.FirstName == "") {
		b.reloadContacts() // Contacts may need to be reloaded
		sender, exists = b.contacts[senderJid]
	}

	if !exists {
		return "someone"
	}

	if exists && sender.FullName != "" {
		return sender.FullName
	}

	if exists && sender.PushName != "" {
		return sender.PushName
	}

	if exists && sender.FirstName != "" {
		return sender.FirstName
	}

	return "someone"
}

func (b *Bwhatsapp) GetProfilePicThumb(jid string) (*types.ProfilePictureInfo, error) {
	pjid, _ := types.ParseJID(jid)

	info, err := b.wc.GetProfilePictureInfo(context.Background(), pjid, &whatsmeow.GetProfilePictureParams{
		Preview: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get avatar: %v", err)
	}

	return info, nil
}

func isGroupJid(identifier string) bool {
	return strings.HasSuffix(identifier, "@g.us") ||
		strings.HasSuffix(identifier, "@temp") ||
		strings.HasSuffix(identifier, "@broadcast")
}

func isPrivateJid(identifier string) bool {
	return strings.HasSuffix(identifier, "@"+types.DefaultUserServer) ||
		strings.HasSuffix(identifier, "@"+types.HiddenUserServer)
}

// resolveChatJID returns the JID under which a message's chat is reported to the
// gateway. WhatsApp increasingly addresses direct messages by LID; when the phone
// number JID for that LID is known, it is returned instead, so that the message
// originates from the phone number channel configured in the gateway and is not
// relayed back to the sender through it. Group chats, phone number chats and LIDs
// with no known phone number are returned unchanged.
func resolveChatJID(info types.MessageInfo, lookup func(types.JID) (types.JID, error)) types.JID {
	if info.IsGroup || info.Chat.Server != types.HiddenUserServer {
		return info.Chat
	}

	if !info.SenderAlt.IsEmpty() && info.SenderAlt.Server == types.DefaultUserServer {
		return info.SenderAlt.ToNonAD()
	}

	pn, err := lookup(info.Chat)
	if err == nil && !pn.IsEmpty() && pn.Server == types.DefaultUserServer {
		return pn.ToNonAD()
	}

	return info.Chat
}

// messageKey is the part of a revoked message's key needed to find its sender.
type messageKey interface {
	GetParticipant() string
}

// deletedMessageSender returns the sender of a revoked message, in the form
// the original message was recorded under. The key names a participant only
// in group chats, where an admin may revoke someone else's message. In a 1:1
// chat the key has no participant and its remote JID is the chat as seen by
// the revoker, so the revoker itself is the original sender.
func deletedMessageSender(key messageKey, revoker types.JID) types.JID {
	participant := key.GetParticipant()
	if participant == "" {
		return revoker
	}

	sender, err := types.ParseJID(participant)
	if err != nil {
		return revoker
	}

	return sender
}

func (b *Bwhatsapp) chatJID(info types.MessageInfo) types.JID {
	return resolveChatJID(info, func(lid types.JID) (types.JID, error) {
		return b.wc.Store.LIDs.GetPNForLID(context.Background(), lid)
	})
}

func (b *Bwhatsapp) getDevice() (*store.Device, error) {
	device := &store.Device{}

	storeContainer, err := sqlstore.New(context.Background(), "sqlite", "file:"+b.Config.GetString("sessionfile")+".db?_pragma=foreign_keys(1)&_pragma=busy_timeout=10000", nil)
	if err != nil {
		return device, fmt.Errorf("failed to connect to database: %v", err)
	}

	device, err = storeContainer.GetFirstDevice(context.Background())
	if err != nil {
		return device, fmt.Errorf("failed to get device: %v", err)
	}

	return device, nil
}

func (b *Bwhatsapp) getNewReplyContext(parentID string) (*proto.ContextInfo, error) {
	replyInfo, err := b.parseMessageID(parentID)
	if err != nil {
		return nil, err
	}

	sender := fmt.Sprintf("%s@%s", replyInfo.Sender.User, replyInfo.Sender.Server)
	ctx := &proto.ContextInfo{
		StanzaID:      &replyInfo.MessageID,
		Participant:   &sender,
		QuotedMessage: &proto.Message{Conversation: new("")}, //nolint: staticcheck
	}

	return ctx, nil
}

func (b *Bwhatsapp) parseMessageID(id string) (*Replyable, error) {
	// No message ID in case action is executed on a message sent before the bridge was started
	// and then the bridge cache doesn't have this message ID mapped
	if id == "" {
		return &Replyable{MessageID: id}, nil
	}

	replyInfo := strings.Split(id, "/")

	if len(replyInfo) == 2 {
		sender, err := types.ParseJID(replyInfo[0])

		if err == nil {
			return &Replyable{
				MessageID: types.MessageID(replyInfo[1]),
				Sender:    sender,
			}, nil
		}
	}

	err := fmt.Errorf("MessageID does not match format of {senderJID}:{messageID} : \"%s\"", id)

	return &Replyable{MessageID: id}, err
}

// quotedMessageID returns the bridge message ID of the message quoted in ci,
// or "" when there is no quote or it cannot be parsed. Messages the bridge sent
// are recorded under the account's phone number JID, but a client that
// addresses the account by LID quotes them with the LID as participant, so the
// account's own LID is translated back to its phone number JID to make the two
// keys match. Every other participant is used as is.
func quotedMessageID(ci *waE2E.ContextInfo, ownID, ownLID types.JID) string {
	if ci.GetStanzaID() == "" || ci.GetParticipant() == "" {
		return ""
	}

	sender, err := types.ParseJID(ci.GetParticipant())
	if err != nil {
		return ""
	}

	if sender.Server == types.HiddenUserServer && !ownID.IsEmpty() && !ownLID.IsEmpty() && sender.User == ownLID.User {
		sender = ownID
	}

	return getMessageIdFormat(sender, ci.GetStanzaID())
}

func (b *Bwhatsapp) parentIDFromContext(ci *waE2E.ContextInfo) string {
	ownID := types.EmptyJID
	if b.wc.Store.ID != nil {
		ownID = *b.wc.Store.ID
	}

	return quotedMessageID(ci, ownID, b.wc.Store.GetLID())
}

func getMessageIdFormat(jid types.JID, messageID string) string {
	// we're crafting our own JID str as AD JID format messes with how stuff looks on a webclient
	jidStr := fmt.Sprintf("%s@%s", jid.User, jid.Server)
	return fmt.Sprintf("%s/%s", jidStr, messageID)
}
