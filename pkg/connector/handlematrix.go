package connector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

var (
	reMxReply  = regexp.MustCompile(`(?is)<mx-reply>.*?</mx-reply>`)
	reUserLink = regexp.MustCompile(`(?is)<a\b[^>]*\bhref="https://matrix\.to/#/(@[^"/?]+)"[^>]*>(.*?)</a>`)
	reGUID     = regexp.MustCompile(`[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}`)
)

// How long Microsoft lets a user edit a message is not documented as a limit; none is enforced here.
const readGap = 5 * time.Second

type outMention struct {
	ID          int    `json:"id"`
	MentionText string `json:"mentionText"`
	Mentioned   struct {
		User struct {
			ID               string `json:"id"`
			DisplayName      string `json:"displayName"`
			UserIdentityType string `json:"userIdentityType"`
		} `json:"user"`
	} `json:"mentioned"`
}

// teamsMarkup is what the Matrix user wrote as Teams HTML. Links to Teams ghosts become <at> mentions.
func (tc *TeamsClient) teamsMarkup(content *event.MessageEventContent) (string, []outMention) {
	if content.Format != event.FormatHTML || content.FormattedBody == "" {
		return strings.ReplaceAll(html.EscapeString(content.Body), "\n", "<br>"), nil
	}
	var mentions []outMention
	markup := reMxReply.ReplaceAllString(content.FormattedBody, "")
	markup = reUserLink.ReplaceAllStringFunc(markup, func(s string) string {
		m := reUserLink.FindStringSubmatch(s)
		mxid, _ := url.PathUnescape(m[1])
		name := strings.TrimPrefix(strings.TrimSpace(format.HTMLToText(m[2])), "@")
		uid, ok := tc.Main.Bridge.Matrix.ParseGhostMXID(id.UserID(mxid))
		if !ok {
			if id.UserID(mxid) != tc.UserLogin.UserMXID {
				return html.EscapeString(name)
			}
			uid = tc.ownID()
		}
		var om outMention
		om.ID, om.MentionText = len(mentions), name
		om.Mentioned.User.ID, om.Mentioned.User.DisplayName, om.Mentioned.User.UserIdentityType = string(uid), name, "aadUser"
		mentions = append(mentions, om)
		return fmt.Sprintf(`<at id="%d">%s</at>`, om.ID, html.EscapeString(name))
	})
	return markup, mentions
}

type outBody struct {
	Body struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	Mentions       []outMention      `json:"mentions,omitempty"`
	Attachments    []graphAttachment `json:"attachments,omitempty"`
	HostedContents []map[string]any  `json:"hostedContents,omitempty"`
}

func chatOf(portal *bridgev2.Portal) string { return string(portal.ID) }

func (tc *TeamsClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	chat := chatOf(msg.Portal)
	content := msg.Content
	body := &outBody{}
	body.Body.ContentType = "html"
	switch content.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
		body.Body.Content, body.Mentions = tc.teamsMarkup(content)
		if content.MsgType == event.MsgEmote {
			body.Body.Content = "<em>" + body.Body.Content + "</em>"
		}
	case event.MsgImage, event.MsgFile, event.MsgVideo, event.MsgAudio:
		if err := tc.attachFile(ctx, chat, content, body); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w %s", bridgev2.ErrUnsupportedMessageType, content.MsgType)
	}
	if msg.ReplyTo != nil {
		if err := tc.attachReply(ctx, chat, msg.ReplyTo, body); err != nil {
			return nil, err
		}
	}
	var sent graphMessage
	if err := tc.Graph.Do(ctx, http.MethodPost, "/chats/"+url.PathEscape(chat)+"/messages", body, &sent); err != nil {
		return nil, err
	}
	return &bridgev2.MatrixMessageResponse{DB: &database.Message{
		ID:        makeMessageID(chat, sent.ID),
		SenderID:  tc.ownID(),
		Timestamp: sent.CreatedDateTime,
		Metadata:  &MessageMetadata{EditedAt: sent.editStamp()},
	}, StreamOrder: sent.CreatedDateTime.UnixMilli()}, nil
}

func (tc *TeamsClient) attachReply(ctx context.Context, chat string, replyTo *database.Message, body *outBody) error {
	_, quoted := parseMessageID(replyTo.ID)
	var original graphMessage
	if err := tc.Graph.Do(ctx, http.MethodGet, "/chats/"+url.PathEscape(chat)+"/messages/"+url.PathEscape(quoted), nil, &original); err != nil {
		return fmt.Errorf("failed to read the message being replied to: %w", err)
	}
	preview, _ := tc.cleanHTML(&original)
	text := []rune(format.HTMLToText(preview))
	if len(text) > 200 {
		text = text[:200]
	}
	ref, _ := json.Marshal(map[string]any{"messageId": quoted, "messagePreview": string(text), "messageSender": original.From})
	body.Attachments = append(body.Attachments, graphAttachment{ID: quoted, ContentType: "messageReference", Content: string(ref)})
	body.Body.Content = fmt.Sprintf(`<attachment id="%s"></attachment>`, quoted) + body.Body.Content
	return nil
}

func (tc *TeamsClient) attachFile(ctx context.Context, chat string, content *event.MessageEventContent, body *outBody) error {
	name := content.FileName
	if name == "" {
		name = content.Body
	}
	if name == "" {
		name = "file"
	}
	kind := "application/octet-stream"
	if content.Info != nil && content.Info.MimeType != "" {
		kind = content.Info.MimeType
	}
	uri := content.URL
	if content.File != nil {
		uri = content.File.URL
	}
	data, err := tc.Main.Bridge.Bot.DownloadMedia(ctx, uri, content.File)
	if err != nil {
		return fmt.Errorf("%w: %w", bridgev2.ErrMediaDownloadFailed, err)
	}
	if int64(len(data)) > tc.Main.Config.MaxFileSize {
		return fmt.Errorf("%w: the file is larger than %d MB", bridgev2.ErrMediaTooLarge, tc.Main.Config.MaxFileSize>>20)
	}
	if content.FileName != "" && content.Body != content.FileName {
		caption := *content
		caption.FileName = ""
		body.Body.Content, body.Mentions = tc.teamsMarkup(&caption)
	}
	if content.MsgType == event.MsgImage && len(data) < hostedImageMax {
		body.HostedContents = append(body.HostedContents, map[string]any{
			"@microsoft.graph.temporaryId": "1",
			"contentBytes":                 base64.StdEncoding.EncodeToString(data),
			"contentType":                  kind,
		})
		body.Body.Content += `<img src="../hostedContents/1/$value" alt="image">`
		return nil
	}
	item, err := tc.uploadDrive(ctx, chat, name, data)
	if err != nil {
		return fmt.Errorf("%w: %w", bridgev2.ErrMediaReuploadFailed, err)
	}
	guid := strings.ToLower(reGUID.FindString(item.ETag))
	if guid == "" {
		guid = strings.ToLower(item.ID)
	}
	body.Attachments = append(body.Attachments, graphAttachment{ID: guid, ContentType: "reference", ContentURL: item.WebURL, Name: item.Name})
	body.Body.Content += fmt.Sprintf(`<attachment id="%s"></attachment>`, guid)
	return nil
}

type driveItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	ETag   string `json:"eTag"`
	WebURL string `json:"webUrl"`
}

// uploadDrive puts a file into the user's OneDrive folder "Microsoft Teams Chat Files", as Teams does,
// and lets the chat's other people read it.
func (tc *TeamsClient) uploadDrive(ctx context.Context, chat, name string, data []byte) (*driveItem, error) {
	path := "/me/drive/root:/" + url.PathEscape("Microsoft Teams Chat Files") + "/" + url.PathEscape(name)
	var item driveItem
	if len(data) <= 4<<20 {
		if err := tc.Graph.Do(ctx, http.MethodPut, path+":/content?@microsoft.graph.conflictBehavior=rename", data, &item); err != nil {
			return nil, err
		}
	} else {
		var session struct {
			UploadURL string `json:"uploadUrl"`
		}
		req := map[string]any{"item": map[string]any{"@microsoft.graph.conflictBehavior": "rename"}}
		if err := tc.Graph.Do(ctx, http.MethodPost, path+":/createUploadSession", req, &session); err != nil {
			return nil, err
		}
		if err := tc.uploadSession(ctx, session.UploadURL, data, &item); err != nil {
			return nil, err
		}
	}
	var members graphPage[graphMember]
	if err := tc.Graph.Do(ctx, http.MethodGet, "/chats/"+url.PathEscape(chat)+"/members", nil, &members); err != nil {
		return nil, err
	}
	var recipients []map[string]string
	for _, m := range members.Value {
		if m.Email != "" && makeUserID(m.UserID) != tc.ownID() {
			recipients = append(recipients, map[string]string{"email": m.Email})
		}
	}
	if len(recipients) > 0 {
		invite := map[string]any{"recipients": recipients, "requireSignIn": true, "sendInvitation": false, "roles": []string{"read"}}
		if err := tc.Graph.Do(ctx, http.MethodPost, "/me/drive/items/"+url.PathEscape(item.ID)+"/invite", invite, nil); err != nil {
			return nil, err
		}
	}
	return &item, nil
}

// uploadSession sends a large file in 5 MiB pieces. The upload URL is pre-authenticated: no token goes there.
func (tc *TeamsClient) uploadSession(ctx context.Context, uploadURL string, data []byte, item *driveItem) error {
	const chunk = 5 << 20 // a multiple of 320 KiB, as OneDrive asks
	for start := 0; start < len(data); start += chunk {
		end := min(start+chunk, len(data))
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data[start:end]))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(data)))
		resp, err := tc.Main.HTTP.Do(req)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("upload answered %d", resp.StatusCode)
		}
		if end == len(data) {
			return json.Unmarshal(raw, item)
		}
	}
	return errors.New("empty file")
}

func (tc *TeamsClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	chat, target := parseMessageID(msg.EditTarget.ID)
	if msg.EditTarget.PartID != "" {
		return fmt.Errorf("%w: only the text of a Teams message can be edited", bridgev2.ErrEditsNotSupportedInPortal)
	}
	body := &outBody{}
	body.Body.ContentType = "html"
	body.Body.Content, body.Mentions = tc.teamsMarkup(msg.Content)
	path := "/chats/" + url.PathEscape(chat) + "/messages/" + url.PathEscape(target)
	if err := tc.Graph.Do(ctx, http.MethodPatch, path, body, nil); err != nil {
		return err
	}
	// Remember Teams' new edit time, so the poller does not bring the edit back as a remote one.
	var current graphMessage
	if err := tc.Graph.Do(ctx, http.MethodGet, path, nil, &current); err == nil {
		msg.EditTarget.Metadata.(*MessageMetadata).EditedAt = current.editStamp()
	}
	return nil
}

func (tc *TeamsClient) PreHandleMatrixReaction(_ context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	emoji := reactionEmoji(reactionType(msg.Content.RelatesTo.Key))
	return bridgev2.MatrixReactionPreResponse{
		SenderID: tc.ownID(),
		EmojiID:  networkid.EmojiID(reactionType(emoji)),
		Emoji:    emoji,
	}, nil
}

func (tc *TeamsClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	chat, target := parseMessageID(msg.TargetMessage.ID)
	path := "/chats/" + url.PathEscape(chat) + "/messages/" + url.PathEscape(target) + "/setReaction"
	return nil, tc.Graph.Do(ctx, http.MethodPost, path, map[string]string{"reactionType": string(msg.PreHandleResp.EmojiID)}, nil)
}

func (tc *TeamsClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	chat, target := parseMessageID(msg.TargetReaction.MessageID)
	path := "/chats/" + url.PathEscape(chat) + "/messages/" + url.PathEscape(target) + "/unsetReaction"
	return tc.Graph.Do(ctx, http.MethodPost, path, map[string]string{"reactionType": string(msg.TargetReaction.EmojiID)}, nil)
}

func (tc *TeamsClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	chat, target := parseMessageID(msg.TargetMessage.ID)
	path := "/users/" + url.PathEscape(string(tc.ownID())) + "/chats/" + url.PathEscape(chat) + "/messages/" + url.PathEscape(target) + "/softDelete"
	return tc.Graph.Do(ctx, http.MethodPost, path, nil, nil)
}

// HandleMatrixReadReceipt marks the chat read in Teams only once the user has read its newest message
// from someone else, and only if every recent message from others is already in the room:
// Teams' read state is per chat, so an earlier receipt would mark unseen messages as seen.
func (tc *TeamsClient) HandleMatrixReadReceipt(ctx context.Context, msg *bridgev2.MatrixReadReceipt) error {
	chat := chatOf(msg.Portal)
	tc.readLock.Lock()
	if time.Since(tc.lastRead[chat]) < readGap {
		tc.readLock.Unlock()
		return nil
	}
	tc.readLock.Unlock()
	var page graphPage[graphMessage]
	if err := tc.Graph.Do(ctx, http.MethodGet, "/chats/"+url.PathEscape(chat)+"/messages?$top=50&$orderby=createdDateTime%20desc", nil, &page); err != nil {
		return err
	}
	var newest *graphMessage
	for i := range page.Value {
		m := &page.Value[i]
		if m.MessageType != "message" || m.DeletedDateTime != nil || m.senderID() == "" || makeUserID(m.senderID()) == tc.ownID() {
			continue
		}
		part, err := tc.Main.Bridge.DB.Message.GetFirstPartByID(ctx, tc.UserLogin.ID, makeMessageID(chat, m.ID))
		if err != nil {
			return err
		} else if part == nil {
			return nil // not bridged yet
		}
		if newest == nil {
			newest = m
		}
	}
	if newest == nil || msg.ReadUpTo.Before(newest.CreatedDateTime.Truncate(time.Millisecond)) {
		return nil
	}
	tc.readLock.Lock()
	tc.lastRead[chat] = time.Now()
	tc.readLock.Unlock()
	req := map[string]any{"user": map[string]string{"id": string(tc.ownID()), "tenantId": tc.meta().TenantID}}
	return tc.Graph.Do(ctx, http.MethodPost, "/chats/"+url.PathEscape(chat)+"/markChatReadForUser", req, nil)
}
