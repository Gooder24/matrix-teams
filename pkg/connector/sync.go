package connector

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

// How many recently active chats are rechecked on every poll, besides those whose newest message moved:
// edits, reactions and deletions do not move a chat's lastMessagePreview.
const recheckActive = 2

func (tc *TeamsClient) pollLoop(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	healthy := false
	for {
		wait := tc.Main.Config.PollInterval
		err := tc.poll(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Warn().Err(err).Msg("Teams poll failed")
			tc.reportError(err)
			healthy = false
			var te ThrottledError
			if errors.As(err, &te) && te.RetryAfter > wait {
				wait = te.RetryAfter
			}
			if errors.Is(err, ErrAuth) {
				return
			}
		case !healthy:
			healthy = true
			tc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (tc *TeamsClient) poll(ctx context.Context) error {
	var page graphPage[graphChat]
	err := tc.Graph.Do(ctx, http.MethodGet, "/me/chats?$expand=lastMessagePreview&$orderby=lastMessagePreview/createdDateTime%20desc&$top=50", nil, &page)
	if err != nil {
		return err
	}
	meta := tc.meta()
	if meta.Chats == nil {
		meta.Chats = make(map[string]*ChatState)
	}
	initialCutoff := time.Now().AddDate(0, 0, -tc.Main.Config.InitialDays)
	recheck := make(map[string]bool)
	for i := 0; i < recheckActive && i < len(page.Value); i++ {
		recheck[page.Value[i].ID] = true
	}
	if len(page.Value) > 0 {
		recheck[page.Value[tc.rotate%len(page.Value)].ID] = true
		tc.rotate++
	}
	changed := false
	for i := range page.Value {
		chat := &page.Value[i]
		if chat.LastMessagePreview == nil {
			continue
		}
		preview := stamp(chat.LastMessagePreview.CreatedDateTime)
		st := meta.Chats[chat.ID]
		if st == nil {
			st = &ChatState{}
			meta.Chats[chat.ID] = st
			changed = true
			if chat.LastMessagePreview.CreatedDateTime.Before(initialCutoff) {
				// An old chat gets a room once somebody writes in it again.
				st.Preview, st.Cursor = preview, preview
				continue
			}
			if err = tc.syncNewChat(ctx, chat, st); err != nil {
				return err
			}
			st.Preview = preview
			continue
		}
		if st.Preview == preview && !recheck[chat.ID] {
			continue
		}
		if err = tc.syncChanges(ctx, chat.ID, st); err != nil {
			return err
		}
		if st.Preview != preview {
			st.Preview = preview
			changed = true
		}
	}
	if changed {
		return tc.UserLogin.Save(ctx)
	}
	return nil
}

func (tc *TeamsClient) syncNewChat(ctx context.Context, chat *graphChat, st *ChatState) error {
	info, err := tc.chatInfo(ctx, chat)
	if err != nil {
		return err
	}
	tc.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			PortalKey:    makePortalKey(chat.ID, tc.UserLogin.ID),
			CreatePortal: true,
		},
		ChatInfo: info,
	})
	var page graphPage[graphMessage]
	n := tc.Main.Config.InitialMessages
	if n <= 0 {
		n = 20
	}
	err = tc.Graph.Do(ctx, http.MethodGet, "/chats/"+url.PathEscape(chat.ID)+"/messages?$top="+itoa(n)+"&$orderby=createdDateTime%20desc", nil, &page)
	if err != nil {
		return err
	}
	cutoff := time.Now().AddDate(0, 0, -tc.Main.Config.InitialDays)
	var msgs []graphMessage
	for _, m := range page.Value {
		if m.CreatedDateTime.After(cutoff) {
			msgs = append(msgs, m)
		}
	}
	return tc.handleMessages(ctx, chat.ID, st, msgs)
}

// syncChanges reads every message of the chat modified since the cursor: new ones, edits, deletions and reactions.
func (tc *TeamsClient) syncChanges(ctx context.Context, chatID string, st *ChatState) error {
	path := "/chats/" + url.PathEscape(chatID) + "/messages?$top=50&$orderby=lastModifiedDateTime%20desc"
	if st.Cursor != "" {
		path += "&$filter=lastModifiedDateTime%20gt%20" + url.QueryEscape(st.Cursor)
	}
	var msgs []graphMessage
	for pages := 0; path != "" && pages < 10; pages++ {
		var page graphPage[graphMessage]
		if err := tc.Graph.Do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return err
		}
		msgs = append(msgs, page.Value...)
		if st.Cursor == "" {
			break
		}
		path = page.NextLink
	}
	return tc.handleMessages(ctx, chatID, st, msgs)
}

func (tc *TeamsClient) handleMessages(ctx context.Context, chatID string, st *ChatState, msgs []graphMessage) error {
	sort.Slice(msgs, func(i, j int) bool {
		if !msgs[i].CreatedDateTime.Equal(msgs[j].CreatedDateTime) {
			return msgs[i].CreatedDateTime.Before(msgs[j].CreatedDateTime)
		}
		return msgs[i].ID < msgs[j].ID
	})
	cursor := st.Cursor
	for i := range msgs {
		msg := &msgs[i]
		if err := tc.handleMessage(ctx, chatID, msg); err != nil {
			return err
		}
		if s := stamp(msg.LastModifiedDateTime); s > cursor {
			cursor = s
		}
	}
	if cursor != st.Cursor {
		st.Cursor = cursor
		return tc.UserLogin.Save(ctx)
	}
	return nil
}

func (tc *TeamsClient) handleMessage(ctx context.Context, chatID string, msg *graphMessage) error {
	key := makePortalKey(chatID, tc.UserLogin.ID)
	msgID := makeMessageID(chatID, msg.ID)
	existing, err := tc.Main.Bridge.DB.Message.GetAllPartsByID(ctx, tc.UserLogin.ID, msgID)
	if err != nil {
		return err
	}
	base := simplevent.EventMeta{
		PortalKey:   key,
		Sender:      tc.sender(msg.senderID()),
		Timestamp:   msg.CreatedDateTime,
		StreamOrder: msg.CreatedDateTime.UnixMilli(),
	}
	if msg.MessageType == "systemEventMessage" {
		detail, text, ok := callLog(msg)
		if !ok || len(existing) > 0 {
			return nil
		}
		if detail.Initiator != nil && detail.Initiator.User != nil {
			base.Sender = tc.sender(detail.Initiator.User.ID)
		}
		base.Type, base.CreatePortal = bridgev2.RemoteEventMessage, true
		tc.UserLogin.QueueRemoteEvent(&simplevent.Message[string]{
			EventMeta: base, ID: msgID, Data: text,
			ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, text string) (*bridgev2.ConvertedMessage, error) {
				return convertCallLog(text), nil
			},
		})
		return nil
	}
	if msg.MessageType != "message" || msg.senderID() == "" {
		return nil
	}
	switch {
	case msg.DeletedDateTime != nil:
		if len(existing) > 0 {
			base.Type = bridgev2.RemoteEventMessageRemove
			tc.UserLogin.QueueRemoteEvent(&simplevent.MessageRemove{EventMeta: base, TargetMessage: msgID})
		}
		return nil
	case len(existing) == 0:
		base.Type, base.CreatePortal = bridgev2.RemoteEventMessage, true
		defer func() {
			if len(msg.Reactions) > 0 {
				tc.queueReactions(key, msgID, msg)
			}
		}()
		tc.UserLogin.QueueRemoteEvent(&simplevent.Message[*graphMessage]{
			EventMeta: base, ID: msgID, Data: msg,
			ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *graphMessage) (*bridgev2.ConvertedMessage, error) {
				return tc.convertMessage(ctx, portal, intent, chatID, data)
			},
		})
	case msg.editStamp() != "" && msg.editStamp() != existing[0].Metadata.(*MessageMetadata).EditedAt:
		base.Type = bridgev2.RemoteEventEdit
		if msg.LastEditedDateTime != nil {
			base.Timestamp = *msg.LastEditedDateTime
		}
		tc.UserLogin.QueueRemoteEvent(&simplevent.Message[*graphMessage]{
			EventMeta: base, ID: msgID, TargetMessage: msgID, Data: msg,
			ConvertEditFunc: tc.convertEdit,
		})
	}
	if len(existing) > 0 {
		tc.queueReactions(key, msgID, msg)
	}
	return nil
}

func (tc *TeamsClient) convertEdit(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, msg *graphMessage) (*bridgev2.ConvertedEdit, error) {
	var textPart *database.Message
	for _, part := range existing {
		if part.PartID == "" {
			textPart = part
		}
	}
	for _, part := range existing {
		part.Metadata.(*MessageMetadata).EditedAt = msg.editStamp()
	}
	content := tc.textPart(msg)
	if textPart == nil || content == nil {
		return nil, bridgev2.ErrIgnoringRemoteEvent
	}
	return &bridgev2.ConvertedEdit{ModifiedParts: []*bridgev2.ConvertedEditPart{{
		Part: textPart, Type: event.EventMessage, Content: content, NewMentions: content.Mentions,
	}}}, nil
}

// queueReactions hands bridgev2 the message's whole reaction list; it adds and removes the difference.
func (tc *TeamsClient) queueReactions(key networkid.PortalKey, msgID networkid.MessageID, msg *graphMessage) {
	data := &bridgev2.ReactionSyncData{Users: make(map[networkid.UserID]*bridgev2.ReactionSyncUser), HasAllUsers: true}
	for _, r := range msg.Reactions {
		uid := r.userID()
		if uid == "" || r.ReactionType == "" {
			continue
		}
		sender := tc.sender(uid)
		user := data.Users[sender.Sender]
		if user == nil {
			user = &bridgev2.ReactionSyncUser{HasAllReactions: true}
			data.Users[sender.Sender] = user
		}
		emoji := reactionEmoji(r.ReactionType)
		user.Reactions = append(user.Reactions, &bridgev2.BackfillReaction{
			Timestamp: r.CreatedDateTime,
			Sender:    sender,
			EmojiID:   networkid.EmojiID(reactionType(emoji)),
			Emoji:     emoji,
		})
	}
	tc.UserLogin.QueueRemoteEvent(&simplevent.ReactionSync{
		EventMeta:     simplevent.EventMeta{Type: bridgev2.RemoteEventReactionSync, PortalKey: key},
		TargetMessage: msgID,
		Reactions:     data,
	})
}
