package connector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.mau.fi/util/random"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
)

const (
	LoginFlowIDBrowser = "browser"
	LoginStepIDPaste   = "io.github.gooder24.teams.login.paste_url"
	LoginStepIDDone    = "io.github.gooder24.teams.login.complete"
	redirectURI        = "http://localhost"
	// The tenant of personal Microsoft accounts, which have no Teams chats in Graph.
	consumersTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"
)

func (tc *TeamsConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Microsoft sign-in",
		Description: "Sign in on Microsoft's page and paste the localhost address it sends you to",
		ID:          LoginFlowIDBrowser,
	}}
}

func (tc *TeamsConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != LoginFlowIDBrowser {
		return nil, fmt.Errorf("unknown login flow %q", flowID)
	}
	return &TeamsLogin{Main: tc, User: user}, nil
}

type TeamsLogin struct {
	Main     *TeamsConnector
	User     *bridgev2.User
	state    string
	verifier string
}

var _ bridgev2.LoginProcessUserInput = (*TeamsLogin)(nil)

func (tl *TeamsLogin) authorizeURL() string {
	sum := sha256.Sum256([]byte(tl.verifier))
	q := url.Values{
		"client_id":             {tl.Main.Config.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"response_mode":         {"query"},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {tl.state},
		"prompt":                {"select_account"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	return strings.TrimRight(tl.Main.Config.LoginURL, "/") + "/" + tl.Main.Config.Tenant + "/oauth2/v2.0/authorize?" + q.Encode()
}

func (tl *TeamsLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	tl.state = random.String(24)
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	tl.verifier = base64.RawURLEncoding.EncodeToString(buf)
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepIDPaste,
		Instructions: "Open this address, sign in with your work or school account and accept. " +
			"The browser then shows an error page on http://localhost: copy that page's whole address and paste it here.\n\n" +
			tl.authorizeURL(),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:        bridgev2.LoginInputFieldTypeURL,
				ID:          "redirect_url",
				Name:        "Address of the localhost page",
				Description: "It starts with http://localhost/?code=",
				Placeholder: "http://localhost/?code=…",
			}},
		},
	}, nil
}

func (tl *TeamsLogin) Cancel() {}

// LoginError is a refusal the user can act on.
type LoginError struct{ Code, Message string }

func (e LoginError) Error() string { return e.Message }

func loginError(code, description string) error {
	has := func(ids ...string) bool {
		for _, id := range ids {
			if strings.Contains(description, id) {
				return true
			}
		}
		return false
	}
	switch {
	case has("AADSTS65001", "AADSTS90094", "AADSTS900941"):
		return LoginError{"admin_consent", "Your organization only allows this app after an administrator approves it (Entra ID → Enterprise applications → Permissions)."}
	case has("AADSTS53003", "AADSTS53000"):
		return LoginError{"conditional_access", "Your organization's Conditional Access policies block this sign-in."}
	case code == "access_denied" || code == "consent_required":
		return LoginError{"denied", "Microsoft did not grant access; accept the permissions at the end of the sign-in."}
	case code == "invalid_client" || code == "unauthorized_client" || has("AADSTS700016", "AADSTS7000218"):
		return LoginError{"bad_client", "The configured client_id is not a public client with the redirect URI http://localhost."}
	case code == "interaction_required":
		return LoginError{"interaction_required", "Microsoft wants you to sign in again (for example new multi-factor authentication)."}
	case code == "invalid_grant":
		return LoginError{"invalid_grant", "The sign-in code was already used or is too old; start the login again."}
	}
	if code == "" {
		return LoginError{"unreachable", "Microsoft did not answer the sign-in; try again later."}
	}
	return LoginError{code, "Microsoft refused the sign-in: " + code}
}

// idClaims reads the unverified payload of the ID token. It came straight from
// the token endpoint over TLS, so it is only used to learn the tenant and object ID.
func idClaims(token string) (tid, oid string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return
	}
	var c struct {
		TID string `json:"tid"`
		OID string `json:"oid"`
	}
	_ = json.Unmarshal(data, &c)
	return c.TID, c.OID
}

func (tl *TeamsLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	pasted := strings.TrimSpace(input["redirect_url"])
	u, err := url.Parse(pasted)
	if err != nil || u.Query().Get("state") == "" && u.Query().Get("code") == "" && u.Query().Get("error") == "" {
		return nil, LoginError{"bad_url", "That is not the address of the localhost page; copy the whole address bar."}
	}
	q := u.Query()
	if q.Get("state") != tl.state {
		return nil, LoginError{"bad_state", "That address belongs to a different sign-in; use the newest link."}
	}
	if e := q.Get("error"); e != "" {
		return nil, loginError(e, q.Get("error_description"))
	}
	tok, err := tl.Main.postToken(ctx, tl.Main.Config.Tenant, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"redirect_uri":  {redirectURI},
		"code_verifier": {tl.verifier},
		"scope":         {strings.Join(scopes, " ")},
	})
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(tok.Scope), "chat.readwrite") || tok.RefreshToken == "" {
		return nil, LoginError{"scopes", "Microsoft did not grant access to Teams chats (Chat.ReadWrite)."}
	}
	tid, oid := idClaims(tok.IDToken)
	if tid == consumersTenant {
		return nil, LoginError{"personal", "Personal Microsoft accounts have no Teams chats this bridge can reach; use a work or school account."}
	}
	if tid == "" || oid == "" {
		return nil, LoginError{"no_id", "Microsoft did not say which account signed in."}
	}
	meta := &UserLoginMetadata{TenantID: tid}
	graph := tl.Main.newGraph(meta, nil)
	graph.apply(tok)
	var me graphUser
	if err = graph.Do(ctx, http.MethodGet, "/me?$select=id,displayName,mail,userPrincipalName", nil, &me); err != nil {
		return nil, fmt.Errorf("failed to read your profile: %w", err)
	}
	if err = graph.Do(ctx, http.MethodGet, "/me/chats?$top=1", nil, nil); err != nil {
		return nil, LoginError{"no_teams", "Teams chats are not reachable for this account: " + err.Error()}
	}
	ul, err := tl.User.NewLogin(ctx, &database.UserLogin{
		ID:         networkid.UserLoginID(makeUserID(oid)),
		RemoteName: me.DisplayName,
		RemoteProfile: status.RemoteProfile{
			Name:  me.DisplayName,
			Email: me.address(),
		},
		Metadata: meta,
	}, &bridgev2.NewLoginParams{DeleteOnConflict: true})
	if err != nil {
		return nil, fmt.Errorf("failed to save the login: %w", err)
	}
	ul.Client.Connect(ul.Log.WithContext(tl.Main.Bridge.BackgroundCtx))
	return &bridgev2.LoginStep{
		Type:           bridgev2.LoginStepTypeComplete,
		StepID:         LoginStepIDDone,
		Instructions:   "Signed in as " + me.DisplayName,
		CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: ul.ID, UserLogin: ul},
	}, nil
}
