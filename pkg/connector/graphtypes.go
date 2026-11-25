package connector

import (
	"encoding/json"
	"time"
)

type graphUser struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

func (u graphUser) address() string {
	if u.Mail != "" {
		return u.Mail
	}
	return u.UserPrincipalName
}

type identity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type identitySet struct {
	User *identity `json:"user"`
}

type graphChat struct {
	ID                 string `json:"id"`
	Topic              string `json:"topic"`
	ChatType           string `json:"chatType"` // oneOnOne, group, meeting
	LastMessagePreview *struct {
		ID              string    `json:"id"`
		CreatedDateTime time.Time `json:"createdDateTime"`
	} `json:"lastMessagePreview"`
}

type graphMember struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

type graphAttachment struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType"`
	ContentURL  string `json:"contentUrl,omitempty"`
	Content     string `json:"content,omitempty"`
	Name        string `json:"name,omitempty"`
}

type graphMention struct {
	ID          int    `json:"id"`
	MentionText string `json:"mentionText"`
	Mentioned   struct {
		User *identity `json:"user"`
	} `json:"mentioned"`
}

type graphReaction struct {
	ReactionType    string    `json:"reactionType"`
	CreatedDateTime time.Time `json:"createdDateTime"`
	User            struct {
		ID   string    `json:"id"`
		User *identity `json:"user"`
	} `json:"user"`
}

func (r graphReaction) userID() string {
	if r.User.User != nil && r.User.User.ID != "" {
		return r.User.User.ID
	}
	return r.User.ID
}

type graphMessage struct {
	ID                   string       `json:"id"`
	MessageType          string       `json:"messageType"`
	CreatedDateTime      time.Time    `json:"createdDateTime"`
	LastModifiedDateTime time.Time    `json:"lastModifiedDateTime"`
	LastEditedDateTime   *time.Time   `json:"lastEditedDateTime"`
	DeletedDateTime      *time.Time   `json:"deletedDateTime"`
	Subject              string       `json:"subject"`
	From                 *identitySet `json:"from"`
	Body                 struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	Attachments []graphAttachment `json:"attachments"`
	Mentions    []graphMention    `json:"mentions"`
	Reactions   []graphReaction   `json:"reactions"`
	EventDetail json.RawMessage   `json:"eventDetail"`
}

func (m *graphMessage) senderID() string {
	if m.From != nil && m.From.User != nil {
		return m.From.User.ID
	}
	return ""
}

func (m *graphMessage) editStamp() string {
	if m.LastEditedDateTime == nil {
		return ""
	}
	return m.LastEditedDateTime.UTC().Format(time.RFC3339Nano)
}

type graphPage[T any] struct {
	Value    []T    `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}
