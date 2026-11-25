package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
)

func (tc *TeamsClient) userResponse(ctx context.Context, u graphUser) (*bridgev2.ResolveIdentifierResponse, error) {
	uid := makeUserID(u.ID)
	ghost, err := tc.Main.Bridge.GetGhostByID(ctx, uid)
	if err != nil {
		return nil, err
	}
	info := &bridgev2.UserInfo{Name: ptr.Ptr(u.DisplayName)}
	if addr := u.address(); addr != "" {
		info.Identifiers = []string{"mailto:" + addr}
	}
	return &bridgev2.ResolveIdentifierResponse{Ghost: ghost, UserID: uid, UserInfo: info}, nil
}

// SearchUsers looks the query up in the organization's directory by name or address.
func (tc *TeamsClient) SearchUsers(ctx context.Context, query string) ([]*bridgev2.ResolveIdentifierResponse, error) {
	query = strings.ReplaceAll(strings.TrimSpace(query), `"`, "")
	if query == "" {
		return nil, nil
	}
	search := fmt.Sprintf(`"displayName:%s" OR "mail:%s"`, query, query)
	var page graphPage[graphUser]
	path := "/users?$top=20&$select=id,displayName,mail,userPrincipalName&$search=" + url.QueryEscape(search)
	if err := tc.Graph.Do(ctx, http.MethodGet, path, nil, &page, "ConsistencyLevel", "eventual"); err != nil {
		return nil, err
	}
	out := make([]*bridgev2.ResolveIdentifierResponse, 0, len(page.Value))
	for _, u := range page.Value {
		if makeUserID(u.ID) == tc.ownID() {
			continue
		}
		resp, err := tc.userResponse(ctx, u)
		if err != nil {
			return nil, err
		}
		out = append(out, resp)
	}
	return out, nil
}

// ResolveIdentifier takes an Entra object ID, an e-mail address or a user principal name.
func (tc *TeamsClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	identifier = strings.TrimPrefix(strings.TrimSpace(identifier), "mailto:")
	var u graphUser
	err := tc.Graph.Do(ctx, http.MethodGet, "/users/"+url.PathEscape(identifier)+"?$select=id,displayName,mail,userPrincipalName", nil, &u)
	var ge GraphError
	if errors.As(err, &ge) && ge.Status == 404 {
		return nil, bridgev2.WrapRespErrManual(fmt.Errorf("nobody is called %q in your organization's directory", identifier), "M_NOT_FOUND", 404)
	} else if err != nil {
		return nil, err
	}
	resp, err := tc.userResponse(ctx, u)
	if err != nil || !createChat {
		return resp, err
	}
	resp.Chat, err = tc.createDM(ctx, u.ID)
	return resp, err
}

func (tc *TeamsClient) CreateChatWithGhost(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.CreateChatResponse, error) {
	return tc.createDM(ctx, string(ghost.ID))
}

// createDM opens (or finds, Graph returns the existing one) the one-to-one chat with a person.
func (tc *TeamsClient) createDM(ctx context.Context, oid string) (*bridgev2.CreateChatResponse, error) {
	member := func(who string) map[string]any {
		return map[string]any{
			"@odata.type":     "#microsoft.graph.aadUserConversationMember",
			"roles":           []string{"owner"},
			"user@odata.bind": strings.TrimRight(tc.Main.Config.GraphURL, "/") + "/users('" + who + "')",
		}
	}
	req := map[string]any{"chatType": "oneOnOne", "members": []any{member(string(tc.ownID())), member(oid)}}
	var chat graphChat
	if err := tc.Graph.Do(ctx, http.MethodPost, "/chats", req, &chat); err != nil {
		return nil, err
	}
	info, err := tc.chatInfo(ctx, &chat)
	if err != nil {
		return nil, err
	}
	return &bridgev2.CreateChatResponse{PortalKey: makePortalKey(chat.ID, tc.UserLogin.ID), PortalInfo: info}, nil
}
