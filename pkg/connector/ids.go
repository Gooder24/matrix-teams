package connector

import (
	"strconv"
	"strings"

	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// UserLoginMetadata keeps the OAuth tokens and, per chat, how far the bridge has read.
type UserLoginMetadata struct {
	TenantID     string                `json:"tenant_id"`
	AccessToken  string                `json:"access_token"`
	RefreshToken string                `json:"refresh_token"`
	Expiry       jsontime.Unix         `json:"expiry"`
	Scope        string                `json:"scope"`
	Chats        map[string]*ChatState `json:"chats,omitempty"`
}

// ChatState is what the poller remembers about one Teams chat.
type ChatState struct {
	// Newest lastModifiedDateTime seen in the chat (RFC 3339), the cursor for the next change query.
	Cursor string `json:"cursor,omitempty"`
	// createdDateTime of the chat's lastMessagePreview when it was last synced.
	Preview string `json:"preview,omitempty"`
}

// MessageMetadata is stored for every bridged part.
type MessageMetadata struct {
	EditedAt string `json:"edited_at,omitempty"`
}

func makeUserID(oid string) networkid.UserID { return networkid.UserID(strings.ToLower(oid)) }

func makePortalKey(chatID string, login networkid.UserLoginID) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(chatID), Receiver: login}
}

// Teams message IDs are only unique inside a chat, so the chat ID is part of the message ID.
func makeMessageID(chatID, msgID string) networkid.MessageID {
	return networkid.MessageID(chatID + "|" + msgID)
}

func parseMessageID(id networkid.MessageID) (chatID, msgID string) {
	chatID, msgID, _ = strings.Cut(string(id), "|")
	return
}

func itoa(n int) string { return strconv.Itoa(n) }
