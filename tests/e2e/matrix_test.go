package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// mx is a tiny Matrix client for the test user: it syncs, joins every invite and keeps every event.
type mx struct {
	t      *testing.T
	base   string
	token  string
	userID string

	mu     sync.Mutex
	events map[string][]map[string]any // room → state and timeline events, in order
	stop   chan struct{}
}

func (c *mx) call(method, path string, body any, out any) error {
	var reader io.Reader
	if b, ok := body.([]byte); ok {
		reader = bytes.NewReader(b)
	} else if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *mx) register(name, regToken string) {
	body := map[string]any{"username": name, "password": "test-password-" + name, "inhibit_login": false}
	var first struct {
		Session string `json:"session"`
	}
	data, _ := json.Marshal(body)
	resp, err := http.Post(c.base+"/_matrix/client/v3/register", "application/json", bytes.NewReader(data))
	if err != nil {
		c.t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&first)
	resp.Body.Close()
	body["auth"] = map[string]any{"type": "m.login.registration_token", "token": regToken, "session": first.Session}
	var out struct {
		AccessToken string `json:"access_token"`
		UserID      string `json:"user_id"`
	}
	if err = c.call("POST", "/_matrix/client/v3/register", body, &out); err != nil {
		c.t.Fatal(err)
	}
	c.token, c.userID = out.AccessToken, out.UserID
}

func (c *mx) syncLoop() {
	since := ""
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		var resp struct {
			NextBatch string `json:"next_batch"`
			Rooms     struct {
				Join map[string]struct {
					State    struct{ Events []map[string]any } `json:"state"`
					Timeline struct{ Events []map[string]any } `json:"timeline"`
				} `json:"join"`
				Invite map[string]any `json:"invite"`
			} `json:"rooms"`
		}
		q := "/_matrix/client/v3/sync?timeout=1000"
		if since != "" {
			q += "&since=" + url.QueryEscape(since)
		}
		if err := c.call("GET", q, nil, &resp); err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		since = resp.NextBatch
		c.mu.Lock()
		for room, r := range resp.Rooms.Join {
			c.events[room] = append(c.events[room], r.State.Events...)
			c.events[room] = append(c.events[room], r.Timeline.Events...)
		}
		c.mu.Unlock()
		for room := range resp.Rooms.Invite {
			_ = c.call("POST", "/_matrix/client/v3/join/"+url.PathEscape(room), map[string]any{}, nil)
		}
	}
}

func (c *mx) roomEvents(room string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.events[room]...)
}

func str(v any, keys ...string) string {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	s, _ := v.(string)
	return s
}

// roomWhere finds a joined room that has an event for which match is true.
func (c *mx) roomWhere(match func(evt map[string]any) bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for room, evts := range c.events {
		for _, e := range evts {
			if match(e) {
				return room
			}
		}
	}
	return ""
}

func (c *mx) find(room string, match func(evt map[string]any) bool) map[string]any {
	for _, e := range c.roomEvents(room) {
		if match(e) {
			return e
		}
	}
	return nil
}

func (c *mx) send(room, evtType string, content map[string]any) string {
	var out struct {
		EventID string `json:"event_id"`
	}
	txn := fmt.Sprintf("t%d", time.Now().UnixNano())
	if err := c.call("PUT", "/_matrix/client/v3/rooms/"+url.PathEscape(room)+"/send/"+evtType+"/"+txn, content, &out); err != nil {
		c.t.Error(err)
	}
	return out.EventID
}

func (c *mx) upload(data []byte, name, mime string) string {
	req, _ := http.NewRequest("POST", c.base+"/_matrix/media/v3/upload?filename="+url.QueryEscape(name), bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mime)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		ContentURI string `json:"content_uri"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.ContentURI
}

func isBody(text string) func(map[string]any) bool {
	return func(e map[string]any) bool {
		return e["type"] == "m.room.message" && strings.Contains(str(e, "content", "body"), text)
	}
}
