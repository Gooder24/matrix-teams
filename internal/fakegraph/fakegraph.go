// Package fakegraph is a stand-in for Microsoft's token endpoint and the Microsoft Graph
// calls the bridge makes, for tests. Nothing reaches Microsoft. State lives in memory;
// the exported methods change it (new messages, edits, deletions, reactions) and Log
// shows every write the bridge made.
package fakegraph

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Tenant = "tenant-1"
	Me     = "aaaaaaaa-0000-0000-0000-000000000001"
	Bea    = "bbbbbbbb-0000-0000-0000-000000000002"
	Cyril  = "cccccccc-0000-0000-0000-000000000003"
	Dana   = "dddddddd-0000-0000-0000-000000000004"

	ChatDM    = "19:dm-bea@unq.gbl.spaces"
	ChatGroup = "19:group@thread.v2"
	ChatOld   = "19:old@thread.v2"
)

// PNG is a 1×1 picture, served as photos and hosted images.
var PNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

type person struct{ ID, Name, Mail string }

var people = []person{
	{Me, "Alex Owner", "alex@school.test"},
	{Bea, "Bea Novak", "bea@school.test"},
	{Cyril, "Cyril Old", "cyril@school.test"},
	{Dana, "Dana New", "dana@school.test"},
}

func who(id string) person {
	for _, p := range people {
		if p.ID == id {
			return p
		}
	}
	return person{ID: id}
}

type chat struct {
	ID, Topic, Type string
	Members         []string
}

// Entry is one write the bridge made.
type Entry struct {
	Method, Path string
	Body         map[string]any
	Size         int
	Grant        string
}

type Server struct {
	*httptest.Server
	mu        sync.Mutex
	n         int64
	chats     map[string]*chat
	messages  map[string][]map[string]any
	tokens    map[string]bool
	refresh   map[string]bool
	ExpiresIn int
	log       []Entry
	drive     map[string][]byte
	uploads   map[string]*upload
	Fail      map[string]int // "METHOD path-regex" → status
}

type upload struct {
	name string
	data []byte
}

func (s *Server) G() string { return s.URL + "/v1.0" }

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (s *Server) newID() string {
	s.n++
	return strconv.FormatInt(time.Now().UnixMilli()*100+s.n%100, 10)
}

func user(id string) map[string]any {
	p := who(id)
	return map[string]any{"application": nil, "device": nil, "user": map[string]any{"id": id, "displayName": p.Name, "userIdentityType": "aadUser"}}
}

func (s *Server) message(chatID, from, html string, at time.Time) map[string]any {
	t := stamp(at)
	return map[string]any{
		"id": s.newID(), "messageType": "message", "createdDateTime": t, "lastModifiedDateTime": t,
		"lastEditedDateTime": nil, "deletedDateTime": nil, "chatId": chatID, "from": user(from),
		"body":        map[string]any{"contentType": "html", "content": html},
		"attachments": []any{}, "reactions": []any{}, "mentions": []any{},
	}
}

// New starts the fake with three chats: a DM with Bea (with one message of three weeks ago) and a group from today, and a group
// whose last message is 40 days old.
func New() *Server {
	s := &Server{
		chats: map[string]*chat{
			ChatDM:    {ChatDM, "", "oneOnOne", []string{Me, Bea}},
			ChatGroup: {ChatGroup, "Physics project", "group", []string{Me, Bea, Cyril}},
			ChatOld:   {ChatOld, "Old group", "group", []string{Me, Cyril}},
		},
		messages:  map[string][]map[string]any{},
		tokens:    map[string]bool{},
		refresh:   map[string]bool{},
		ExpiresIn: 3600,
		drive:     map[string][]byte{},
		uploads:   map[string]*upload{},
		Fail:      map[string]int{},
	}
	now := time.Now()
	s.messages[ChatDM] = []map[string]any{
		// Older than initial_days: never bridged, yet still in the chat for the read receipt to see.
		s.message(ChatDM, Bea, "<p>three weeks ago</p>", now.Add(-21*24*time.Hour)),
		s.message(ChatDM, Bea, `<p>Hi, do you have the <b>homework</b>? <emoji alt="😀"></emoji></p>`, now.Add(-time.Hour)),
		s.message(ChatDM, Me, "<p>I do, sending it tonight</p>", now.Add(-50*time.Minute)),
	}
	s.messages[ChatGroup] = []map[string]any{s.message(ChatGroup, Cyril, "<div>Who makes the slides?</div>", now.Add(-2*time.Hour))}
	s.messages[ChatOld] = []map[string]any{s.message(ChatOld, Cyril, "<p>long ago</p>", now.Add(-40*24*time.Hour))}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *Server) find(chatID, id string) map[string]any {
	for _, m := range s.messages[chatID] {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

func touch(m map[string]any) { m["lastModifiedDateTime"] = stamp(time.Now()) }

// Say adds a message from someone else. extra is merged into the message.
func (s *Server) Say(chatID, from, html string, extra map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.message(chatID, from, html, time.Now())
	for k, v := range extra {
		m[k] = v
	}
	s.messages[chatID] = append(s.messages[chatID], m)
	return m
}

// HostedImage is the src of an inline image of the message with that ID.
func (s *Server) HostedImage(chatID, msgID string) string {
	return fmt.Sprintf(`<img src="%s/chats/%s/messages/%s/hostedContents/aWQ9MQ==/$value">`, s.G(), chatID, msgID)
}

// System adds a system note, for example a call that ended.
func (s *Server) System(chatID string, detail map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.message(chatID, Me, "<systemEventMessage/>", time.Now())
	m["messageType"], m["from"], m["eventDetail"] = "systemEventMessage", nil, detail
	s.messages[chatID] = append(s.messages[chatID], m)
}

func (s *Server) Edit(chatID, id, html string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(chatID, id)
	m["body"] = map[string]any{"contentType": "html", "content": html}
	m["lastEditedDateTime"] = stamp(time.Now())
	touch(m)
}

func (s *Server) Delete(chatID, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(chatID, id)
	m["deletedDateTime"] = stamp(time.Now())
	touch(m)
}

func (s *Server) React(chatID, id, userID, kind string, remove bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(chatID, id)
	s.setReaction(m, userID, kind, remove)
}

func (s *Server) setReaction(m map[string]any, userID, kind string, remove bool) {
	var out []any
	for _, r := range m["reactions"].([]any) {
		r := r.(map[string]any)
		if r["reactionType"] == kind && r["user"].(map[string]any)["user"].(map[string]any)["id"] == userID {
			continue
		}
		out = append(out, r)
	}
	if !remove {
		out = append(out, map[string]any{"reactionType": kind, "createdDateTime": stamp(time.Now()), "user": user(userID)})
	}
	if out == nil {
		out = []any{}
	}
	m["reactions"] = out
	touch(m)
}

// Messages is a copy of a chat's messages.
func (s *Server) Messages(chatID string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.messages[chatID])
	var out []map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func (s *Server) Log() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Entry(nil), s.log...)
}

// Revoke makes every token stop working, as when the user removes the app.
func (s *Server) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens, s.refresh = map[string]bool{}, map[string]bool{}
}

func (s *Server) send(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func jwt(claims map[string]any) string {
	enc := func(v any) string { d, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(d) }
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".x"
}

func (s *Server) issue(scope string) map[string]any {
	access, refresh := "at-"+s.newID(), "rt-"+s.newID()
	s.tokens[access], s.refresh[refresh] = true, true
	return map[string]any{
		"token_type": "Bearer", "access_token": access, "refresh_token": refresh, "expires_in": s.ExpiresIn, "scope": scope,
		"id_token": jwt(map[string]any{"oid": Me, "tid": Tenant, "name": "Alex Owner"}),
	}
}

func (s *Server) token(w http.ResponseWriter, form url.Values) {
	s.log = append(s.log, Entry{Method: "TOKEN", Grant: form.Get("grant_type")})
	switch form.Get("grant_type") {
	case "authorization_code":
		switch {
		case form.Get("code") == "consent":
			s.send(w, 400, map[string]any{"error": "invalid_grant", "error_description": "AADSTS65001: The user or administrator has not consented"})
		case form.Get("code_verifier") == "" || form.Get("redirect_uri") != "http://localhost":
			s.send(w, 400, map[string]any{"error": "invalid_request", "error_description": "AADSTS50148: verifier"})
		case form.Get("code") == "good":
			s.send(w, 200, s.issue(form.Get("scope")))
		default:
			s.send(w, 400, map[string]any{"error": "invalid_grant", "error_description": "AADSTS70008: expired"})
		}
	case "refresh_token":
		if !s.refresh[form.Get("refresh_token")] {
			s.send(w, 400, map[string]any{"error": "invalid_grant", "error_description": "AADSTS700082: The refresh token has expired"})
			return
		}
		delete(s.refresh, form.Get("refresh_token"))
		s.send(w, 200, s.issue(form.Get("scope")))
	default:
		s.send(w, 400, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (s *Server) chatView(c *chat) map[string]any {
	var last map[string]any
	for _, m := range s.messages[c.ID] {
		if m["deletedDateTime"] == nil && (last == nil || m["createdDateTime"].(string) > last["createdDateTime"].(string)) {
			last = m
		}
	}
	v := map[string]any{"id": c.ID, "topic": nil, "chatType": c.Type, "lastMessagePreview": nil}
	if c.Topic != "" {
		v["topic"] = c.Topic
	}
	if last != nil {
		v["lastMessagePreview"] = map[string]any{"id": last["id"], "createdDateTime": last["createdDateTime"]}
	}
	return v
}

var (
	reChat      = regexp.MustCompile(`^/chats/([^/]+)$`)
	reMembers   = regexp.MustCompile(`^/chats/([^/]+)/members$`)
	reMessages  = regexp.MustCompile(`^/chats/([^/]+)/messages$`)
	reMessage   = regexp.MustCompile(`^/chats/([^/]+)/messages/([^/]+)$`)
	reReaction  = regexp.MustCompile(`^/chats/([^/]+)/messages/([^/]+)/(setReaction|unsetReaction)$`)
	reRead      = regexp.MustCompile(`^/chats/([^/]+)/markChatReadForUser$`)
	reDelete    = regexp.MustCompile(`^/users/([^/]+)/chats/([^/]+)/messages/([^/]+)/softDelete$`)
	reHosted    = regexp.MustCompile(`^/chats/([^/]+)/messages/([^/]+)/hostedContents/([^/]+)/\$value$`)
	reShare     = regexp.MustCompile(`^/shares/([^/]+)/driveItem$`)
	reDrivePut  = regexp.MustCompile(`^/me/drive/root:/(.+):/content$`)
	reDriveSess = regexp.MustCompile(`^/me/drive/root:/(.+):/createUploadSession$`)
	reInvite    = regexp.MustCompile(`^/me/drive/items/([^/]+)/invite$`)
	rePhoto     = regexp.MustCompile(`^/users/([^/]+)/photos/96x96/\$value$`)
	reUser      = regexp.MustCompile(`^/users/([^/]+)$`)
	reSearch    = regexp.MustCompile(`displayName:([^"]+)"`)
	reBind      = regexp.MustCompile(`users\('([^']+)'\)`)
	reFilter    = regexp.MustCompile(`lastModifiedDateTime gt (\S+)`)
	reRange     = regexp.MustCompile(`bytes (\d+)-(\d+)/(\d+)`)
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/oauth2/v2.0/token"):
		form, _ := url.ParseQuery(string(raw))
		s.token(w, form)
		return
	case strings.HasPrefix(path, "/upload/"):
		s.uploadPart(w, r, strings.TrimPrefix(path, "/upload/"), raw)
		return
	case strings.HasPrefix(path, "/dl/"):
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(s.drive[strings.TrimPrefix(path, "/dl/")])
		return
	case !strings.HasPrefix(path, "/v1.0/"):
		s.send(w, 404, nil)
		return
	}
	if !s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		s.send(w, 401, map[string]any{"error": map[string]any{"code": "InvalidAuthenticationToken", "message": "expired"}})
		return
	}
	path = strings.TrimPrefix(path, "/v1.0")
	for pattern, code := range s.Fail {
		if regexp.MustCompile(pattern).MatchString(r.Method + " " + path) {
			if code == 429 {
				w.Header().Set("Retry-After", "1")
			}
			s.send(w, code, map[string]any{"error": map[string]any{"code": "Fail", "message": "test refusal"}})
			return
		}
	}
	var body map[string]any
	if strings.Contains(r.Header.Get("Content-Type"), "json") {
		_ = json.Unmarshal(raw, &body)
	}
	if r.Method != http.MethodGet {
		s.log = append(s.log, Entry{Method: r.Method, Path: path, Body: body, Size: len(raw)})
	}
	s.graph(w, r, path, body, raw)
}

func (s *Server) graph(w http.ResponseWriter, r *http.Request, path string, body map[string]any, raw []byte) {
	q := r.URL.Query()
	get := r.Method == http.MethodGet
	if m := reChat.FindStringSubmatch(path); m != nil && get && s.chats[m[1]] != nil {
		s.send(w, 200, s.chatView(s.chats[m[1]]))
		return
	}
	switch {
	case path == "/me" && get:
		s.send(w, 200, map[string]any{"id": Me, "displayName": "Alex Owner", "mail": "alex@school.test", "userPrincipalName": "alex@school.test"})
	case path == "/me/chats":
		var out []map[string]any
		for _, c := range s.chats {
			out = append(out, s.chatView(c))
		}
		preview := func(v map[string]any) string {
			if p, ok := v["lastMessagePreview"].(map[string]any); ok {
				return p["createdDateTime"].(string)
			}
			return ""
		}
		sort.Slice(out, func(i, j int) bool { return preview(out[i]) > preview(out[j]) })
		s.send(w, 200, map[string]any{"value": out})
	case reRead.MatchString(path):
		u, _ := body["user"].(map[string]any)
		if u["id"] != Me || u["tenantId"] != Tenant {
			s.send(w, 400, map[string]any{"error": map[string]any{"code": "BadRequest"}})
			return
		}
		s.send(w, 204, nil)
	case reMembers.MatchString(path):
		c := s.chats[reMembers.FindStringSubmatch(path)[1]]
		var out []map[string]any
		for _, id := range c.Members {
			p := who(id)
			out = append(out, map[string]any{"userId": id, "displayName": p.Name, "email": p.Mail})
		}
		s.send(w, 200, map[string]any{"value": out})
	case reMessages.MatchString(path) && get:
		msgs := append([]map[string]any(nil), s.messages[reMessages.FindStringSubmatch(path)[1]]...)
		if f := reFilter.FindStringSubmatch(q.Get("$filter")); f != nil {
			var kept []map[string]any
			for _, m := range msgs {
				if m["lastModifiedDateTime"].(string) > f[1] {
					kept = append(kept, m)
				}
			}
			msgs = kept
		}
		key := "createdDateTime"
		if strings.Contains(q.Get("$orderby"), "lastModified") {
			key = "lastModifiedDateTime"
		}
		sort.Slice(msgs, func(i, j int) bool { return msgs[i][key].(string) > msgs[j][key].(string) })
		if top, _ := strconv.Atoi(q.Get("$top")); top > 0 && len(msgs) > top {
			msgs = msgs[:top]
		}
		if msgs == nil {
			msgs = []map[string]any{}
		}
		s.send(w, 200, map[string]any{"value": msgs})
	case reMessages.MatchString(path):
		chatID := reMessages.FindStringSubmatch(path)[1]
		content := body["body"].(map[string]any)["content"].(string)
		m := s.message(chatID, Me, content, time.Now())
		if a, ok := body["attachments"].([]any); ok {
			m["attachments"] = a
		}
		if ms, ok := body["mentions"].([]any); ok {
			m["mentions"] = ms
		}
		if _, ok := body["hostedContents"]; ok {
			m["body"].(map[string]any)["content"] = strings.ReplaceAll(content, `src="../hostedContents/1/$value"`, `src="`+s.G()+"/chats/"+chatID+"/messages/"+m["id"].(string)+`/hostedContents/aWQ9MQ==/$value"`)
		}
		s.messages[chatID] = append(s.messages[chatID], m)
		s.send(w, 201, m)
	case reReaction.MatchString(path):
		g := reReaction.FindStringSubmatch(path)
		s.setReaction(s.find(g[1], g[2]), Me, body["reactionType"].(string), g[3] == "unsetReaction")
		s.send(w, 204, nil)
	case reMessage.MatchString(path):
		g := reMessage.FindStringSubmatch(path)
		m := s.find(g[1], g[2])
		if m == nil {
			s.send(w, 404, map[string]any{"error": map[string]any{"code": "NotFound"}})
			return
		}
		if r.Method == http.MethodPatch {
			m["body"] = body["body"]
			if a, ok := body["attachments"]; ok {
				m["attachments"] = a
			} else {
				m["attachments"] = []any{}
			}
			m["lastEditedDateTime"] = stamp(time.Now())
			touch(m)
			s.send(w, 204, nil)
			return
		}
		s.send(w, 200, m)
	case reDelete.MatchString(path):
		g := reDelete.FindStringSubmatch(path)
		m := s.find(g[2], g[3])
		m["deletedDateTime"] = stamp(time.Now())
		touch(m)
		s.send(w, 204, nil)
	case reHosted.MatchString(path):
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(PNG)
	case reShare.MatchString(path):
		s.drive["shared-1"] = []byte(strings.Repeat("PDF-DATA", 100))
		s.send(w, 200, map[string]any{"name": "assignment.pdf", "size": 800, "file": map[string]any{"mimeType": "application/pdf"},
			"@microsoft.graph.downloadUrl": s.URL + "/dl/shared-1"})
	case reDrivePut.MatchString(path):
		name, _ := url.PathUnescape(reDrivePut.FindStringSubmatch(path)[1])
		s.send(w, 201, s.driveItem(name[strings.LastIndex(name, "/")+1:], raw))
	case reDriveSess.MatchString(path):
		name, _ := url.PathUnescape(reDriveSess.FindStringSubmatch(path)[1])
		key := s.newID()
		s.uploads[key] = &upload{name: name[strings.LastIndex(name, "/")+1:]}
		s.send(w, 200, map[string]any{"uploadUrl": s.URL + "/upload/" + key})
	case reInvite.MatchString(path):
		s.send(w, 200, map[string]any{"value": []any{}})
	case rePhoto.MatchString(path):
		if rePhoto.FindStringSubmatch(path)[1] != Bea {
			s.send(w, 404, map[string]any{"error": map[string]any{"code": "ImageNotFound"}})
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(PNG)
	case path == "/users":
		want := ""
		if m := reSearch.FindStringSubmatch(q.Get("$search")); m != nil {
			want = strings.ToLower(m[1])
		}
		var out []map[string]any
		for _, p := range people {
			if want != "" && strings.Contains(strings.ToLower(p.Name), want) {
				out = append(out, map[string]any{"id": p.ID, "displayName": p.Name, "mail": p.Mail})
			}
		}
		if r.Header.Get("ConsistencyLevel") != "eventual" {
			s.send(w, 400, map[string]any{"error": map[string]any{"code": "Request_UnsupportedQuery"}})
			return
		}
		s.send(w, 200, map[string]any{"value": out})
	case reUser.MatchString(path):
		key := reUser.FindStringSubmatch(path)[1]
		for _, p := range people {
			if p.ID == key || p.Mail == key {
				s.send(w, 200, map[string]any{"id": p.ID, "displayName": p.Name, "mail": p.Mail})
				return
			}
		}
		s.send(w, 404, map[string]any{"error": map[string]any{"code": "Request_ResourceNotFound"}})
	case path == "/chats" && r.Method == http.MethodPost:
		members := body["members"].([]any)
		other := reBind.FindStringSubmatch(members[1].(map[string]any)["user@odata.bind"].(string))[1]
		id := "19:new-" + other + "@unq.gbl.spaces"
		if s.chats[id] == nil {
			s.chats[id] = &chat{id, "", "oneOnOne", []string{Me, other}}
		}
		s.send(w, 201, s.chatView(s.chats[id]))
	default:
		s.send(w, 404, map[string]any{"error": map[string]any{"code": "NotFound", "message": path}})
	}
}

func (s *Server) driveItem(name string, data []byte) map[string]any {
	id := "item-" + s.newID()
	s.drive[id] = data
	return map[string]any{"id": id, "name": name, "size": len(data), "eTag": `"{1A2B3C4D-1111-2222-3333-444455556666},1"`,
		"webUrl": "https://school-my.sharepoint.com/personal/alex/Documents/Microsoft%20Teams%20Chat%20Files/" + url.PathEscape(name)}
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, key string, raw []byte) {
	up := s.uploads[key]
	s.log = append(s.log, Entry{Method: "UPLOAD", Path: r.Header.Get("Content-Range"), Size: len(raw), Grant: r.Header.Get("Authorization")})
	g := reRange.FindStringSubmatch(r.Header.Get("Content-Range"))
	if up == nil || g == nil {
		s.send(w, 400, nil)
		return
	}
	up.data = append(up.data, raw...)
	end, _ := strconv.Atoi(g[2])
	total, _ := strconv.Atoi(g[3])
	if end+1 < total {
		s.send(w, 202, map[string]any{"nextExpectedRanges": []string{strconv.Itoa(end+1) + "-"}})
		return
	}
	s.send(w, 201, s.driveItem(up.name, up.data))
}
