package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

// Images up to this size go inline as Teams hosted content; larger files go through OneDrive.
const hostedImageMax = 3 << 20

var (
	reEmoji      = regexp.MustCompile(`(?is)<emoji\b[^>]*\balt="([^"]*)"[^>]*>(\s*</emoji>)?`)
	reAttachment = regexp.MustCompile(`(?is)<attachment\b[^>]*>\s*</attachment>`)
	reAt         = regexp.MustCompile(`(?is)<at\b[^>]*\bid="?(\d+)"?[^>]*>(.*?)</at>`)
	reImg        = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	reImgSrc     = regexp.MustCompile(`(?is)<img\b[^>]*\bsrc="([^"]+/hostedContents/[^"]+)"`)
	reHostedImg  = regexp.MustCompile(`(?is)<img\b[^>]*\bsrc="[^"]+/hostedContents/[^"]+"[^>]*>`)
	reDuration   = regexp.MustCompile(`^P(?:(\d+)D)?T?(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)(?:\.\d+)?S)?$`)
)

var teamsReactions = map[string]string{
	"like": "👍", "heart": "❤️", "laugh": "😆", "surprised": "😮", "sad": "😢", "angry": "😡",
}

// reactionEmoji is the emoji Matrix shows for a Teams reaction type.
func reactionEmoji(kind string) string {
	if e, ok := teamsReactions[kind]; ok {
		return e
	}
	switch kind {
	case "❤", "♥️", "♥":
		return "❤️"
	}
	return kind
}

// reactionType is the Teams reaction for an emoji: one of its six classic names, any other emoji as itself.
func reactionType(emoji string) string {
	bare := strings.TrimSuffix(emoji, "\ufe0f")
	if bare == "❤" || bare == "♥" {
		return "heart"
	}
	for k, v := range teamsReactions {
		if strings.TrimSuffix(v, "\ufe0f") == bare {
			return k
		}
	}
	return emoji
}

// cleanHTML turns Teams HTML into Matrix HTML: emoji become text, mentions become
// matrix.to links to ghosts, attachment and image placeholders disappear.
func (tc *TeamsClient) cleanHTML(msg *graphMessage) (string, []id.UserID) {
	markup := msg.Body.Content
	if msg.Body.ContentType != "html" {
		markup = strings.ReplaceAll(html.EscapeString(markup), "\n", "<br>")
	}
	byID := make(map[int]*identity)
	for _, m := range msg.Mentions {
		if m.Mentioned.User != nil {
			byID[m.ID] = m.Mentioned.User
		}
	}
	var mentioned []id.UserID
	markup = reEmoji.ReplaceAllString(markup, "$1")
	markup = reAttachment.ReplaceAllString(markup, "")
	markup = reImg.ReplaceAllString(markup, "")
	markup = reAt.ReplaceAllStringFunc(markup, func(s string) string {
		m := reAt.FindStringSubmatch(s)
		n, _ := strconv.Atoi(m[1])
		who := byID[n]
		if who == nil || who.ID == "" {
			return m[2]
		}
		mxid := tc.Main.Bridge.Matrix.GhostIntent(makeUserID(who.ID)).GetMXID()
		if tc.IsThisUser(context.Background(), makeUserID(who.ID)) {
			if u := tc.UserLogin.UserMXID; u != "" {
				mxid = u
			}
		}
		mentioned = append(mentioned, mxid)
		return fmt.Sprintf(`<a href="%s">%s</a>`, mxid.URI().MatrixToURL(), m[2])
	})
	if msg.Subject != "" {
		markup = "<strong>" + html.EscapeString(msg.Subject) + "</strong><br>" + markup
	}
	return strings.TrimSpace(markup), mentioned
}

func (tc *TeamsClient) textPart(msg *graphMessage) *event.MessageEventContent {
	markup, mentioned := tc.cleanHTML(msg)
	for _, a := range msg.Attachments {
		if a.ContentType != "reference" && a.ContentType != "messageReference" {
			markup += "<br><em>[A Teams card – open it in Teams]</em>"
			break
		}
	}
	body := strings.TrimSpace(format.HTMLToText(markup))
	if body == "" {
		return nil
	}
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body, Mentions: &event.Mentions{UserIDs: mentioned}}
	if markup != html.EscapeString(body) {
		content.Format = event.FormatHTML
		content.FormattedBody = markup
	}
	return content
}

type teamsFile struct {
	name, mime, link string
	data             []byte
}

func (tc *TeamsClient) filesOf(ctx context.Context, msg *graphMessage) []teamsFile {
	log := zerolog.Ctx(ctx)
	limit := tc.Main.Config.MaxFileSize
	var out []teamsFile
	for n, src := range reImgSrc.FindAllStringSubmatch(msg.Body.Content, -1) {
		addr := html.UnescapeString(src[1])
		data, kind, err := tc.Graph.Download(ctx, addr, true, limit)
		ext := ".png"
		if exts, _ := mime.ExtensionsByType(kind); len(exts) > 0 {
			ext = exts[0]
		}
		f := teamsFile{name: fmt.Sprintf("image-%d%s", n+1, ext), mime: kind, data: data}
		if err != nil {
			log.Warn().Err(err).Msg("Failed to download hosted image")
			f.mime = "image/png"
		}
		out = append(out, f)
	}
	for _, a := range msg.Attachments {
		if a.ContentType != "reference" || a.ContentURL == "" {
			continue
		}
		name := a.Name
		if name == "" {
			name = "file"
		}
		kind := mime.TypeByExtension(path.Ext(name))
		if kind == "" {
			kind = "application/octet-stream"
		}
		f := teamsFile{name: name, mime: kind, link: a.ContentURL}
		share := "u!" + base64.RawURLEncoding.EncodeToString([]byte(a.ContentURL))
		var item struct {
			Size int64 `json:"size"`
			File struct {
				MimeType string `json:"mimeType"`
			} `json:"file"`
			DownloadURL string `json:"@microsoft.graph.downloadUrl"`
		}
		err := tc.Graph.Do(ctx, http.MethodGet, "/shares/"+share+"/driveItem?$select=name,size,file,@microsoft.graph.downloadUrl", nil, &item)
		if err == nil && item.DownloadURL != "" && item.Size <= limit {
			// The download URL is pre-authenticated and outside Graph: no token goes there.
			f.data, _, err = tc.Graph.Download(ctx, item.DownloadURL, false, limit)
			if item.File.MimeType != "" {
				f.mime = item.File.MimeType
			}
		}
		if err != nil {
			log.Warn().Err(err).Str("file", name).Msg("Failed to download shared file, sending a link")
			f.data = nil
		}
		out = append(out, f)
	}
	return out
}

func replyTarget(chatID string, msg *graphMessage) *networkid.MessageOptionalPartID {
	for _, a := range msg.Attachments {
		if a.ContentType == "messageReference" && a.ID != "" {
			return &networkid.MessageOptionalPartID{MessageID: makeMessageID(chatID, a.ID)}
		}
	}
	return nil
}

func (tc *TeamsClient) convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, chatID string, msg *graphMessage) (*bridgev2.ConvertedMessage, error) {
	meta := &MessageMetadata{EditedAt: msg.editStamp()}
	cm := &bridgev2.ConvertedMessage{ReplyTo: replyTarget(chatID, msg)}
	if text := tc.textPart(msg); text != nil {
		cm.Parts = append(cm.Parts, &bridgev2.ConvertedMessagePart{ID: "", Type: event.EventMessage, Content: text, DBMetadata: meta})
	}
	for n, f := range tc.filesOf(ctx, msg) {
		part := &bridgev2.ConvertedMessagePart{ID: networkid.PartID(fmt.Sprintf("file-%d", n)), Type: event.EventMessage, DBMetadata: &MessageMetadata{}}
		if f.data == nil {
			body := "[File: " + f.name + "]"
			if f.link != "" {
				body += " " + f.link
			}
			part.Content = &event.MessageEventContent{MsgType: event.MsgNotice, Body: body}
		} else {
			content := &event.MessageEventContent{
				MsgType:  event.MsgFile,
				Body:     f.name,
				FileName: f.name,
				Info:     &event.FileInfo{MimeType: f.mime, Size: len(f.data)},
			}
			switch {
			case strings.HasPrefix(f.mime, "image/"):
				content.MsgType = event.MsgImage
			case strings.HasPrefix(f.mime, "video/"):
				content.MsgType = event.MsgVideo
			case strings.HasPrefix(f.mime, "audio/"):
				content.MsgType = event.MsgAudio
			}
			var err error
			content.URL, content.File, err = intent.UploadMedia(ctx, portal.MXID, f.data, f.name, f.mime)
			if err != nil {
				return nil, fmt.Errorf("failed to upload %s: %w", f.name, err)
			}
			part.Content = content
		}
		cm.Parts = append(cm.Parts, part)
	}
	if len(cm.Parts) == 0 {
		cm.Parts = append(cm.Parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage, DBMetadata: meta,
			Content: &event.MessageEventContent{MsgType: event.MsgNotice, Body: "[Empty message]"},
		})
	}
	return cm, nil
}

type callDetail struct {
	Type          string       `json:"@odata.type"`
	CallEventType string       `json:"callEventType"`
	CallDuration  string       `json:"callDuration"`
	Initiator     *identitySet `json:"initiator"`
}

// callLog reads a Teams note that a call or meeting ended. Graph tells who started it and how long it took.
func callLog(msg *graphMessage) (*callDetail, string, bool) {
	var d callDetail
	if len(msg.EventDetail) == 0 || json.Unmarshal(msg.EventDetail, &d) != nil {
		return nil, "", false
	}
	if !strings.HasSuffix(d.Type, "callEndedEventMessageDetail") || (d.CallEventType != "call" && d.CallEventType != "meeting") {
		return nil, "", false
	}
	text := "Call"
	if m := reDuration.FindStringSubmatch(d.CallDuration); m != nil {
		num := func(s string) int { n, _ := strconv.Atoi(s); return n }
		hours, minutes, seconds := num(m[1])*24+num(m[2]), num(m[3]), num(m[4])
		if hours > 0 {
			text += fmt.Sprintf(" (%d:%02d:%02d)", hours, minutes, seconds)
		} else if minutes > 0 || seconds > 0 {
			text += fmt.Sprintf(" (%d:%02d)", minutes, seconds)
		}
	}
	return &d, text, true
}

func convertCallLog(text string) *bridgev2.ConvertedMessage {
	return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
		Type:       event.EventMessage,
		Content:    &event.MessageEventContent{MsgType: event.MsgText, Body: text},
		Extra:      map[string]any{"com.beeper.action_message": map[string]any{"type": "call", "call_type": ""}},
		DBMetadata: &MessageMetadata{},
	}}}
}
