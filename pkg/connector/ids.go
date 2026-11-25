package connector

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// UserLoginMetadata keeps the OAuth tokens and, per chat, how far the bridge has read.
// The poller, the token refresh and bridgev2's own saves touch it from different goroutines,
// so every read and write holds mu, and so does marshalling. Never call UserLogin.Save with mu held.
type UserLoginMetadata struct {
	mu           sync.Mutex
	TenantID     string               `json:"tenant_id"`
	AccessToken  string               `json:"access_token"`
	RefreshToken string               `json:"refresh_token"`
	Expiry       jsontime.Unix        `json:"expiry"`
	Scope        string               `json:"scope"`
	Chats        map[string]ChatState `json:"chats,omitempty"`
}

type userLoginMetadata UserLoginMetadata

func (m *UserLoginMetadata) MarshalJSON() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return json.Marshal((*userLoginMetadata)(m))
}

func (m *UserLoginMetadata) chat(id string) (ChatState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.Chats[id]
	return st, ok
}

func (m *UserLoginMetadata) setChat(id string, st ChatState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Chats == nil {
		m.Chats = make(map[string]ChatState)
	}
	m.Chats[id] = st
}

// ChatState is what the poller remembers about one Teams chat.
type ChatState struct {
	// Newest lastModifiedDateTime seen in the chat (see stamp), the cursor for the next change query.
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

// stamp writes Graph's times with exactly three decimals, so that stamps compare correctly as strings.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
