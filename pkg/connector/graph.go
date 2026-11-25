package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix/bridgev2"
)

// Scopes the bridge asks for. Chat.ReadWrite is required; the others make
// names, photos, the directory and sending files work.
var scopes = []string{
	"offline_access", "openid", "profile", "email",
	"https://graph.microsoft.com/User.Read",
	"https://graph.microsoft.com/User.ReadBasic.All",
	"https://graph.microsoft.com/Chat.ReadWrite",
	"https://graph.microsoft.com/Files.ReadWrite.All",
}

// ErrAuth means the refresh token no longer works and the user has to sign in again.
var ErrAuth = errors.New("teams sign-in expired")

// ThrottledError is Graph's 429/503/504 with its Retry-After.
type ThrottledError struct{ RetryAfter time.Duration }

func (e ThrottledError) Error() string { return fmt.Sprintf("throttled, retry after %s", e.RetryAfter) }

// GraphError is any other non-2xx answer.
type GraphError struct {
	Status  int
	Code    string
	Message string
}

func (e GraphError) Error() string {
	return fmt.Sprintf("graph %d %s: %s", e.Status, e.Code, e.Message)
}

type Graph struct {
	tc    *TeamsConnector
	meta  *UserLoginMetadata
	login *bridgev2.UserLogin // nil during sign-in
	// refresh lets one refresh run at a time; meta.mu guards the fields themselves.
	refresh sync.Mutex
	saveMu  sync.Mutex
}

// save stores the login. UserLogin.Save is not safe to call from two goroutines at once,
// and the poller and a token refresh for a Matrix event both save.
func (g *Graph) save(ctx context.Context) error {
	if g.login == nil {
		return nil
	}
	g.saveMu.Lock()
	defer g.saveMu.Unlock()
	return g.login.Save(ctx)
}

func (tc *TeamsConnector) newGraph(meta *UserLoginMetadata, login *bridgev2.UserLogin) *Graph {
	return &Graph{tc: tc, meta: meta, login: login}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func (tc *TeamsConnector) tokenURL(tenant string) string {
	return strings.TrimRight(tc.Config.LoginURL, "/") + "/" + tenant + "/oauth2/v2.0/token"
}

func (tc *TeamsConnector) postToken(ctx context.Context, tenant string, form url.Values) (*tokenResponse, error) {
	form.Set("client_id", tc.Config.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tc.tokenURL(tenant), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := tc.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok tokenResponse
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, fmt.Errorf("token endpoint answered %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		return &tok, loginError(tok.Error, tok.Description)
	}
	return &tok, nil
}

// apply stores a token answer. The caller holds g.meta.mu.
func (g *Graph) apply(tok *tokenResponse) {
	g.meta.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		g.meta.RefreshToken = tok.RefreshToken
	}
	if tok.Scope != "" {
		g.meta.Scope = tok.Scope
	}
	g.meta.Expiry = jsontime.U(time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second))
}

// token returns a valid access token, refreshing it a minute before it expires.
func (g *Graph) token(ctx context.Context) (string, error) {
	g.refresh.Lock()
	defer g.refresh.Unlock()
	g.meta.mu.Lock()
	access, refresh, tenant, expiry := g.meta.AccessToken, g.meta.RefreshToken, g.meta.TenantID, g.meta.Expiry.Time
	g.meta.mu.Unlock()
	if access != "" && time.Until(expiry) > time.Minute {
		return access, nil
	}
	if refresh == "" {
		return "", ErrAuth
	}
	tok, err := g.tc.postToken(ctx, tenant, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"scope":         {strings.Join(scopes, " ")},
	})
	if err != nil {
		if tok != nil && (tok.Error == "invalid_grant" || tok.Error == "interaction_required") {
			return "", fmt.Errorf("%w: %v", ErrAuth, err)
		}
		return "", err
	}
	g.meta.mu.Lock()
	g.apply(tok)
	g.meta.mu.Unlock()
	if err = g.save(ctx); err != nil {
		return "", fmt.Errorf("failed to save refreshed token: %w", err)
	}
	return tok.AccessToken, nil
}

func (g *Graph) url(path string) string {
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		return path
	}
	return strings.TrimRight(g.tc.Config.GraphURL, "/") + path
}

// isGraphURL says whether an absolute address belongs to the configured Graph,
// so the access token is never sent anywhere else.
func (g *Graph) isGraphURL(addr string) bool {
	base, err := url.Parse(g.tc.Config.GraphURL)
	if err != nil {
		return false
	}
	u, err := url.Parse(addr)
	return err == nil && u.Scheme == base.Scheme && u.Host == base.Host
}

// Do sends a request to Graph. body may be nil, []byte (sent as is with ctype) or anything JSON.
func (g *Graph) Do(ctx context.Context, method, path string, body any, out any, headers ...string) error {
	resp, err := g.raw(ctx, method, path, body, headers...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *Graph) raw(ctx context.Context, method, path string, body any, headers ...string) (*http.Response, error) {
	token, err := g.token(ctx)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
		ctype = "application/octet-stream"
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
		ctype = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, g.url(path), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := g.tc.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		wait := 30 * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		return nil, ThrottledError{RetryAfter: wait}
	case http.StatusUnauthorized:
		// The token was refused although it had not expired: drop it so the next call refreshes.
		g.meta.mu.Lock()
		if g.meta.AccessToken == token {
			g.meta.AccessToken = ""
		}
		g.meta.mu.Unlock()
	}
	var ge struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ge)
	return nil, GraphError{Status: resp.StatusCode, Code: ge.Error.Code, Message: ge.Error.Message}
}

// Download fetches a file. withToken is only honoured for Graph addresses.
func (g *Graph) Download(ctx context.Context, addr string, withToken bool, limit int64) ([]byte, string, error) {
	var resp *http.Response
	var err error
	if withToken && g.isGraphURL(addr) {
		resp, err = g.raw(ctx, http.MethodGet, addr, nil)
	} else {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
		if err == nil {
			resp, err = g.tc.HTTP.Do(req)
			if err == nil && resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				err = fmt.Errorf("download answered %d", resp.StatusCode)
			}
		}
	}
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, "", fmt.Errorf("file is larger than %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("file is larger than %d bytes", limit)
	}
	return data, resp.Header.Get("Content-Type"), nil
}
