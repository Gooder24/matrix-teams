package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

type TeamsClient struct {
	Main      *TeamsConnector
	UserLogin *bridgev2.UserLogin
	Graph     *Graph

	stop        context.CancelFunc
	wg          sync.WaitGroup
	rotate      int
	readLock    sync.Mutex
	lastRead    map[string]time.Time // chat → when Teams last got a mark
	pendingRead map[string]time.Time // chat → newest receipt that waits for the end of readGap
	names       sync.Map             // ghost ID → display name from chat member lists
	failing     map[string]*failure  // chat → its syncs that failed in a row; poll goroutine only
}

// failure: a chat whose sync keeps failing is tried again on the next poll, then after 1, 3 and from then on 7
// skipped polls, so a chat that is gone or forbidden costs little and one that works again comes back soon.
type failure struct{ times, skip int }

var (
	_ bridgev2.NetworkAPI                    = (*TeamsClient)(nil)
	_ bridgev2.EditHandlingNetworkAPI        = (*TeamsClient)(nil)
	_ bridgev2.ReactionHandlingNetworkAPI    = (*TeamsClient)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI   = (*TeamsClient)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI = (*TeamsClient)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI = (*TeamsClient)(nil)
	_ bridgev2.GhostDMCreatingNetworkAPI     = (*TeamsClient)(nil)
	_ bridgev2.UserSearchingNetworkAPI       = (*TeamsClient)(nil)
)

func (tc *TeamsClient) meta() *UserLoginMetadata { return tc.UserLogin.Metadata.(*UserLoginMetadata) }

func (tc *TeamsClient) ownID() networkid.UserID { return networkid.UserID(tc.UserLogin.ID) }

// Connect starts polling. The poller has its own context, which ends with Disconnect.
func (tc *TeamsClient) Connect(_ context.Context) {
	if tc.stop != nil {
		return
	}
	ctx, stop := context.WithCancel(tc.Main.Bridge.BackgroundCtx)
	tc.stop = stop
	ctx = tc.UserLogin.Log.WithContext(ctx)
	tc.wg.Add(1)
	go func() {
		defer tc.wg.Done()
		tc.pollLoop(ctx)
	}()
}

func (tc *TeamsClient) Disconnect() {
	if tc.stop != nil {
		tc.stop()
		tc.wg.Wait()
		tc.stop = nil
	}
}

func (tc *TeamsClient) IsLoggedIn() bool {
	m := tc.meta()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.RefreshToken != ""
}

// LogoutRemote forgets the tokens. Graph has no call that ends a refresh token of a public client;
// it stops working when the user removes the app at myapps.microsoft.com or changes the password.
func (tc *TeamsClient) LogoutRemote(ctx context.Context) {
	m := tc.meta()
	m.mu.Lock()
	m.AccessToken, m.RefreshToken, m.Chats = "", "", nil
	m.mu.Unlock()
}

func (tc *TeamsClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return userID == tc.ownID()
}

func (tc *TeamsClient) sender(oid string) bridgev2.EventSender {
	uid := makeUserID(oid)
	if uid == tc.ownID() {
		return bridgev2.EventSender{IsFromMe: true, SenderLogin: tc.UserLogin.ID, Sender: uid}
	}
	return bridgev2.EventSender{Sender: uid}
}

func (tc *TeamsClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	chatID := string(portal.ID)
	var chat graphChat
	if err := tc.Graph.Do(ctx, http.MethodGet, "/chats/"+url.PathEscape(chatID), nil, &chat); err != nil {
		return nil, err
	}
	return tc.chatInfo(ctx, &chat)
}

func (tc *TeamsClient) chatInfo(ctx context.Context, chat *graphChat) (*bridgev2.ChatInfo, error) {
	var members graphPage[graphMember]
	if err := tc.Graph.Do(ctx, http.MethodGet, "/chats/"+url.PathEscape(chat.ID)+"/members", nil, &members); err != nil {
		return nil, err
	}
	info := &bridgev2.ChatInfo{
		Members: &bridgev2.ChatMemberList{IsFull: true, MemberMap: make(bridgev2.ChatMemberMap)},
	}
	for _, m := range members.Value {
		if m.UserID == "" {
			continue
		}
		// No UserInfo here: bridgev2 then asks GetUserInfo once per new ghost, which also reads the photo.
		info.Members.MemberMap.Set(bridgev2.ChatMember{EventSender: tc.sender(m.UserID), Membership: event.MembershipJoin})
		if m.DisplayName != "" {
			tc.names.Store(makeUserID(m.UserID), m.DisplayName)
		}
	}
	if chat.ChatType == "oneOnOne" {
		info.Type = ptr.Ptr(database.RoomTypeDM)
		for uid := range info.Members.MemberMap {
			if uid != tc.ownID() {
				info.Members.OtherUserID = uid
			}
		}
	} else {
		info.Type = ptr.Ptr(database.RoomTypeGroupDM)
		// A group without a topic is named by its people, which Matrix clients already do.
		if chat.Topic != "" {
			info.Name = ptr.Ptr(chat.Topic)
		}
	}
	return info, nil
}

func (tc *TeamsClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	var u graphUser
	if err := tc.Graph.Do(ctx, http.MethodGet, "/users/"+url.PathEscape(string(ghost.ID))+"?$select=id,displayName,mail,userPrincipalName", nil, &u); err != nil {
		// Guests and people of other organizations are not in the directory: keep the name the chat gave.
		var ge GraphError
		name, ok := tc.names.Load(ghost.ID)
		if errors.As(err, &ge) && (ge.Status == 404 || ge.Status == 403) && ok {
			return &bridgev2.UserInfo{Name: ptr.Ptr(name.(string)), IsBot: ptr.Ptr(false)}, nil
		} else if errors.As(err, &ge) && ge.Status == 404 {
			return nil, nil
		}
		return nil, err
	}
	info := &bridgev2.UserInfo{Name: ptr.Ptr(u.DisplayName), IsBot: ptr.Ptr(false)}
	if addr := u.address(); addr != "" {
		info.Identifiers = []string{"mailto:" + addr}
	}
	info.Avatar = tc.avatar(ctx, string(ghost.ID))
	return info, nil
}

// avatar reads the person's 96×96 photo. Its hash is the avatar ID, so a new photo is reuploaded.
func (tc *TeamsClient) avatar(ctx context.Context, oid string) *bridgev2.Avatar {
	data, _, err := tc.Graph.Download(ctx, tc.Graph.url("/users/"+url.PathEscape(oid)+"/photos/96x96/$value"), true, 4<<20)
	if err != nil {
		var ge GraphError
		if errors.As(err, &ge) && ge.Status == 404 {
			return &bridgev2.Avatar{ID: "", Remove: true}
		}
		return nil
	}
	sum := sha256.Sum256(data)
	return &bridgev2.Avatar{
		ID:  networkid.AvatarID(hex.EncodeToString(sum[:8])),
		Get: func(ctx context.Context) ([]byte, error) { return data, nil },
	}
}

func (tc *TeamsClient) reportError(err error) {
	if errors.Is(err, ErrAuth) {
		tc.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "teams-auth-expired",
			Message:    "The Teams sign-in expired, log in again",
		})
		return
	}
	var te ThrottledError
	if errors.As(err, &te) {
		return
	}
	tc.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateTransientDisconnect,
		Error:      "teams-unreachable",
		Message:    fmt.Sprintf("Microsoft Graph is not answering: %s", strings.SplitN(err.Error(), "\n", 2)[0]),
	})
}
