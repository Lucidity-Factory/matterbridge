package bwhatsapp

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/matterbridge-org/matterbridge/bridge"
	"github.com/matterbridge-org/matterbridge/bridge/config"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "modernc.org/sqlite" // needed for sqlite
)

const (
	// Account config parameters
	cfgNumber = "Number"
)

// Bwhatsapp Bridge structure keeping all the information needed for relying
type Bwhatsapp struct {
	*bridge.Config

	startedAt    time.Time
	wc           *whatsmeow.Client
	contacts     map[types.JID]types.ContactInfo
	users        map[string]types.ContactInfo
	userAvatars  map[string]string
	joinedGroups []*types.GroupInfo

	// loginCh receives the login result of the current Connect.
	loginCh atomic.Pointer[chan error]
}

type Replyable struct {
	MessageID types.MessageID
	Sender    types.JID
}

// New Create a new WhatsApp bridge. This will be called for each [whatsapp.<server>] entry you have in the config file
func New(cfg *bridge.Config) bridge.Bridger {
	number := cfg.GetString(cfgNumber)

	if number == "" {
		cfg.Log.Fatalf("Missing configuration for WhatsApp bridge: Number")
	}

	b := &Bwhatsapp{
		Config: cfg,

		users:       make(map[string]types.ContactInfo),
		userAvatars: make(map[string]string),
	}

	return b
}

// Connect to WhatsApp. Required implementation of the Bridger interface
func (b *Bwhatsapp) Connect() error {
	device, err := b.getDevice()
	if err != nil {
		return err
	}

	number := b.GetString(cfgNumber)
	if number == "" {
		return errors.New("whatsapp's telephone number need to be configured")
	}

	b.Log.Debugln("Connecting to WhatsApp..")

	b.wc = whatsmeow.NewClient(device, waLog.Stdout("Client", "INFO", true))
	b.wc.AddEventHandler(b.eventHandler)

	var qrChan <-chan whatsmeow.QRChannelItem
	if b.wc.Store.ID == nil {
		qrChan, err = b.wc.GetQRChannel(context.Background())
		if err != nil && !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
			return errors.New("failed to to get QR channel:" + err.Error())
		}
	}

	// eventHandler reports the login result here. The server ends a new
	// pairing with a 515 stream error, after which whatsmeow reconnects and
	// logs in by itself; disconnecting before that makes WhatsApp drop the
	// new device. So nothing is fetched until Connected arrives.
	loginCh := make(chan error, 1)
	b.loginCh.Store(&loginCh)

	err = b.wc.Connect()
	if err != nil {
		return errors.New("failed to connect to WhatsApp: " + err.Error())
	}

	if qrChan != nil {
		err = waitForQR(qrChan, qrDeadline, b.handleQREvent)
		if err != nil {
			return b.failConnect(err)
		}
	}

	err = b.awaitLogin(loginCh)
	if err != nil {
		return b.failConnect(err)
	}

	b.Log.Infoln("WhatsApp connection successful")

	b.contacts, err = b.wc.Store.Contacts.GetAllContacts(context.Background())
	if err != nil {
		return b.failConnect(fmt.Errorf("failed to get contacts: %w", err))
	}

	b.joinedGroups, err = b.wc.GetJoinedGroups(context.Background())
	if err != nil {
		return b.failConnect(fmt.Errorf("failed to get list of joined groups: %w", err))
	}

	b.startedAt = time.Now()

	b.loadUsers()

	return nil
}

// Disconnect is called while reconnecting to the bridge
// Required implementation of the Bridger interface
func (b *Bwhatsapp) Disconnect() error {
	b.wc.Disconnect()

	return nil
}

// JoinChannel Join a WhatsApp group specified in gateway config as channel='number-id@g.us' or channel='Channel name'
// Required implementation of the Bridger interface
// https://github.com/42wim/matterbridge/blob/2cfd880cdb0df29771bf8f31df8d990ab897889d/bridge/bridge.go#L11-L16
func (b *Bwhatsapp) JoinChannel(channel config.ChannelInfo) error {
	byJid := isGroupJid(channel.Name)

	// verify if we are member of the given group
	if byJid {
		gJID, err := types.ParseJID(channel.Name)
		if err != nil {
			return err
		}

		for _, group := range b.joinedGroups {
			if group.JID == gJID {
				return nil
			}
		}
	}

	foundGroups := []string{}

	for _, group := range b.joinedGroups {
		if group.Name == channel.Name {
			foundGroups = append(foundGroups, group.Name)
		}
	}

	switch len(foundGroups) {
	case 0:
		// didn't match any group - print out possibilites
		for _, group := range b.joinedGroups {
			b.Log.Infof("%s %s", group.JID, group.Name)
		}
		return fmt.Errorf("please specify group's JID from the list above instead of the name '%s'", channel.Name)
	case 1:
		return fmt.Errorf("group name might change. Please configure gateway with channel=\"%v\" instead of channel=\"%v\"", foundGroups[0], channel.Name)
	default:
		return fmt.Errorf("there is more than one group with name '%s'. Please specify one of JIDs as channel name: %v", channel.Name, foundGroups)
	}
}

// Post a document message from the bridge to WhatsApp
func (b *Bwhatsapp) PostDocumentMessage(msg config.Message, filetype string) (string, error) {
	groupJID, _ := types.ParseJID(msg.Channel)

	fi := msg.Extra["file"][0].(config.FileInfo)

	caption := msg.Username + fi.Comment

	resp, err := b.wc.Upload(context.Background(), *fi.Data, whatsmeow.MediaDocument)
	if err != nil {
		return "", err
	}

	// Post document message
	var message proto.Message
	var ctx *proto.ContextInfo
	if msg.ParentID != "" {
		ctx, _ = b.getNewReplyContext(msg.ParentID)
	}

	message.DocumentMessage = &proto.DocumentMessage{
		Title:         &fi.Name,
		FileName:      &fi.Name,
		Mimetype:      &filetype,
		Caption:       &caption,
		MediaKey:      resp.MediaKey,
		FileEncSHA256: resp.FileEncSHA256,
		FileSHA256:    resp.FileSHA256,
		FileLength:    new(resp.FileLength),
		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		ContextInfo:   ctx,
	}

	b.Log.Debugf("=> Sending %#v as a document", msg)

	ID := whatsmeow.GenerateMessageID()
	_, err = b.wc.SendMessage(context.TODO(), groupJID, &message, whatsmeow.SendRequestExtra{ID: ID})

	return ID, err
}

// Post an image message from the bridge to WhatsApp
// Handle, for sure image/jpeg, image/png and image/gif MIME types
func (b *Bwhatsapp) PostImageMessage(msg config.Message, filetype string) (string, error) {
	fi := msg.Extra["file"][0].(config.FileInfo)

	caption := msg.Username + fi.Comment

	resp, err := b.wc.Upload(context.Background(), *fi.Data, whatsmeow.MediaImage)
	if err != nil {
		return "", err
	}

	var message proto.Message
	var ctx *proto.ContextInfo
	if msg.ParentID != "" {
		ctx, _ = b.getNewReplyContext(msg.ParentID)
	}

	message.ImageMessage = &proto.ImageMessage{
		Mimetype:      &filetype,
		Caption:       &caption,
		MediaKey:      resp.MediaKey,
		FileEncSHA256: resp.FileEncSHA256,
		FileSHA256:    resp.FileSHA256,
		FileLength:    new(resp.FileLength),
		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		ContextInfo:   ctx,
	}

	b.Log.Debugf("=> Sending %#v as an image", msg)

	return b.sendMessage(msg, &message)
}

// Post a video message from the bridge to WhatsApp
func (b *Bwhatsapp) PostVideoMessage(msg config.Message, filetype string) (string, error) {
	fi := msg.Extra["file"][0].(config.FileInfo)

	caption := msg.Username + fi.Comment

	resp, err := b.wc.Upload(context.Background(), *fi.Data, whatsmeow.MediaVideo)
	if err != nil {
		return "", err
	}

	var message proto.Message
	var ctx *proto.ContextInfo
	if msg.ParentID != "" {
		ctx, _ = b.getNewReplyContext(msg.ParentID)
	}

	message.VideoMessage = &proto.VideoMessage{
		Mimetype:      &filetype,
		Caption:       &caption,
		MediaKey:      resp.MediaKey,
		FileEncSHA256: resp.FileEncSHA256,
		FileSHA256:    resp.FileSHA256,
		FileLength:    new(resp.FileLength),
		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		ContextInfo:   ctx,
	}

	b.Log.Debugf("=> Sending %#v as a video", msg)

	return b.sendMessage(msg, &message)
}

// Post audio inline
func (b *Bwhatsapp) PostAudioMessage(msg config.Message, filetype string) (string, error) {
	groupJID, _ := types.ParseJID(msg.Channel)

	fi := msg.Extra["file"][0].(config.FileInfo)

	resp, err := b.wc.Upload(context.Background(), *fi.Data, whatsmeow.MediaAudio)
	if err != nil {
		return "", err
	}

	var message proto.Message
	var ctx *proto.ContextInfo
	if msg.ParentID != "" {
		ctx, _ = b.getNewReplyContext(msg.ParentID)
	}

	message.AudioMessage = &proto.AudioMessage{
		Mimetype:      &filetype,
		MediaKey:      resp.MediaKey,
		FileEncSHA256: resp.FileEncSHA256,
		FileSHA256:    resp.FileSHA256,
		FileLength:    new(resp.FileLength),
		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		ContextInfo:   ctx,
	}

	b.Log.Debugf("=> Sending %#v as audio", msg)

	ID, err := b.sendMessage(msg, &message)

	var captionMessage proto.Message
	caption := msg.Username + fi.Comment + "\u2B06" // the char on the end is upwards arrow emoji
	captionMessage.Conversation = &caption

	captionID := whatsmeow.GenerateMessageID()
	_, err = b.wc.SendMessage(context.TODO(), groupJID, &captionMessage, whatsmeow.SendRequestExtra{ID: captionID})

	return ID, err
}

// Send a message from the bridge to WhatsApp
func (b *Bwhatsapp) Send(msg config.Message) (string, error) {
	groupJID, _ := types.ParseJID(msg.Channel)

	extendedMsgID, _ := b.parseMessageID(msg.ID)
	msg.ID = extendedMsgID.MessageID

	b.Log.Debugf("=> Receiving %#v", msg)

	// Delete message
	if msg.Event == config.EventMsgDelete {
		if msg.ID == "" {
			// No message ID in case action is executed on a message sent before the bridge was started
			// and then the bridge cache doesn't have this message ID mapped
			return "", nil
		}

		_, err := b.wc.RevokeMessage(context.Background(), groupJID, msg.ID)

		return "", err
	}

	// Edit message
	if msg.ID != "" {
		b.Log.Debugf("updating message with id %s", msg.ID)

		if b.GetString("editsuffix") != "" {
			msg.Text += b.GetString("EditSuffix")
		} else {
			msg.Text += " (edited)"
		}
	}

	// Handle Upload a file
	if msg.Extra["file"] != nil {
		fi := msg.Extra["file"][0].(config.FileInfo)
		filetype := mime.TypeByExtension(filepath.Ext(fi.Name))

		b.Log.Debugf("Extra file is %#v", filetype)

		// TODO: add different types
		// TODO: add webp conversion
		switch filetype {
		case "image/jpeg", "image/png", "image/gif":
			return b.PostImageMessage(msg, filetype)
		case "video/mp4", "video/3gpp": // TODO: Check if codecs are supported by WA
			return b.PostVideoMessage(msg, filetype)
		case "audio/ogg":
			return b.PostAudioMessage(msg, "audio/ogg; codecs=opus") // TODO: Detect if it is actually OPUS
		case "audio/aac", "audio/mp4", "audio/amr", "audio/mpeg":
			return b.PostAudioMessage(msg, filetype)
		default:
			return b.PostDocumentMessage(msg, filetype)
		}
	}

	var message proto.Message
	text := msg.Username + msg.Text

	// If we have a parent ID send an extended message
	if msg.ParentID != "" {
		replyContext, err := b.getNewReplyContext(msg.ParentID)

		if err == nil {
			message = proto.Message{
				ExtendedTextMessage: &proto.ExtendedTextMessage{
					Text:        &text,
					ContextInfo: replyContext,
				},
			}

			return b.sendMessage(msg, &message)
		}
	}

	message.Conversation = &text

	return b.sendMessage(msg, &message)
}

func (b *Bwhatsapp) sendMessage(rmsg config.Message, message *proto.Message) (string, error) {
	groupJID, _ := types.ParseJID(rmsg.Channel)
	ID := whatsmeow.GenerateMessageID()

	_, err := b.wc.SendMessage(context.Background(), groupJID, message, whatsmeow.SendRequestExtra{ID: ID})

	return getMessageIdFormat(*b.wc.Store.ID, ID), err
}
