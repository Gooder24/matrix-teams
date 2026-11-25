package e2e

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Gooder24/matrix-teams/internal/fakegraph"
)

const wait = 30 * time.Second

func TestBridge(t *testing.T) {
	e := setup(t, "1s")
	c, fake := e.c, e.fake

	t.Run("login refusals", func(t *testing.T) {
		if _, err := e.login("good", false); err == nil || !strings.Contains(err.Error(), "different sign-in") {
			t.Errorf("a pasted address of another sign-in was taken: %v", err)
		}
		if _, err := e.login("consent", true); err == nil || !strings.Contains(err.Error(), "administrator") {
			t.Errorf("missing admin consent not explained: %v", err)
		}
	})
	t.Run("login", func(t *testing.T) {
		step, err := e.login("good", true)
		if err != nil || step.Type != "complete" {
			t.Fatalf("login did not complete: %v %+v", err, step)
		}
	})

	dm := ""
	if !until(wait, func() bool { dm = c.roomWhere(isBody("do you have the")); return dm != "" }) {
		t.Fatal("the DM with Bea never came")
	}
	group := c.roomWhere(func(e map[string]any) bool {
		return e["type"] == "m.room.name" && str(e, "content", "name") == "Physics project"
	})
	homework := c.find(dm, isBody("do you have the"))
	homeworkTeams := fake.Messages(fakegraph.ChatDM)[0]["id"].(string)

	t.Run("incoming history", func(t *testing.T) {
		if !strings.Contains(str(homework, "content", "formatted_body"), "<b>homework</b>") || !strings.Contains(str(homework, "content", "body"), "😀") {
			t.Errorf("text, formatting or emoji lost: %v", homework["content"])
		}
		if homework["sender"] != ghost(fakegraph.Bea) {
			t.Errorf("sender %v", homework["sender"])
		}
		own := c.find(dm, isBody("sending it tonight"))
		if own == nil || own["sender"] != ghost(fakegraph.Me) {
			t.Errorf("own Teams message missing or from the wrong sender: %v", own)
		}
		if group == "" || c.find(group, isBody("Who makes the slides")) == nil {
			t.Errorf("the group or its message is missing")
		}
		if c.roomWhere(isBody("long ago")) != "" {
			t.Errorf("a 40 days old chat was bridged")
		}
	})
	t.Run("ghost profile", func(t *testing.T) {
		var p struct {
			Name   string `json:"displayname"`
			Avatar string `json:"avatar_url"`
		}
		ok := until(wait, func() bool {
			return c.call("GET", "/_matrix/client/v3/profile/"+ghost(fakegraph.Bea), nil, &p) == nil && p.Name == "Bea Novak" && p.Avatar != ""
		})
		if !ok {
			t.Errorf("Bea's ghost: %+v", p)
		}
	})
	t.Run("room features", func(t *testing.T) {
		var f map[string]any
		if err := c.call("GET", "/_matrix/client/v3/rooms/"+dm+"/state/com.beeper.room_features/"+url.PathEscape(server+"/teams"), nil, &f); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"reply", "edit", "delete", "reaction"} {
			if v, _ := f[k].(float64); v <= 0 {
				t.Errorf("%s not announced: %v", k, f[k])
			}
		}
		if f["file"] == nil || f["read_receipts"] != true || f["typing_notifications"] == true {
			t.Errorf("files, receipts or typing announced wrongly: %v", f)
		}
	})

	t.Run("incoming mention, photo, file, reply", func(t *testing.T) {
		fake.Say(fakegraph.ChatDM, fakegraph.Bea, `<p>Hey <at id="0">Alex Owner</at>, look</p>`, map[string]any{
			"mentions": []any{map[string]any{"id": 0, "mentionText": "Alex Owner", "mentioned": map[string]any{
				"user": map[string]any{"id": fakegraph.Me, "displayName": "Alex Owner", "userIdentityType": "aadUser"}}}},
		})
		fake.Say(fakegraph.ChatDM, fakegraph.Bea, "<p>the photo</p>"+fake.HostedImage(fakegraph.ChatDM, "x"), nil)
		fake.Say(fakegraph.ChatDM, fakegraph.Bea, `<attachment id="f1"></attachment>`, map[string]any{
			"attachments": []any{map[string]any{"id": "f1", "contentType": "reference", "contentUrl": "https://school.sharepoint.com/x/assignment.pdf", "name": "assignment.pdf"}},
		})
		fake.Say(fakegraph.ChatDM, fakegraph.Bea, `<attachment id="`+homeworkTeams+`"></attachment><p>answering this</p>`, map[string]any{
			"attachments": []any{map[string]any{"id": homeworkTeams, "contentType": "messageReference", "content": "{}"}},
		})
		if !until(wait, func() bool { return c.find(dm, isBody("answering this")) != nil }) {
			t.Fatal("new messages never came")
		}
		mention := c.find(dm, isBody("look"))
		if !strings.Contains(str(mention, "content", "formatted_body"), "https://matrix.to/#/"+c.userID) {
			t.Errorf("mention of the user is not a link to them: %v", mention["content"])
		}
		if c.find(dm, func(e map[string]any) bool { return str(e, "content", "msgtype") == "m.image" }) == nil || c.find(dm, isBody("the photo")) == nil {
			t.Errorf("hosted image or its text missing")
		}
		file := c.find(dm, func(e map[string]any) bool { return str(e, "content", "msgtype") == "m.file" })
		if file == nil || str(file, "content", "body") != "assignment.pdf" || str(file, "content", "info", "mimetype") != "application/pdf" {
			t.Errorf("shared file: %v", file)
		}
		reply := c.find(dm, isBody("answering this"))
		if str(reply, "content", "m.relates_to", "m.in_reply_to", "event_id") != homework["event_id"] {
			t.Errorf("reply does not point to the message: %v", reply["content"])
		}
	})

	t.Run("incoming edit, reaction, deletion, call", func(t *testing.T) {
		msgs := fake.Messages(fakegraph.ChatDM)
		replyTeams := msgs[len(msgs)-1]["id"].(string)
		reply := c.find(dm, isBody("answering this"))
		fake.Edit(fakegraph.ChatDM, replyTeams, "<p>answering this, edited</p>")
		fake.React(fakegraph.ChatDM, homeworkTeams, fakegraph.Bea, "like", false)
		edited := func(e map[string]any) bool {
			return str(e, "content", "m.relates_to", "rel_type") == "m.replace" && str(e, "content", "m.relates_to", "event_id") == reply["event_id"]
		}
		if !until(wait, func() bool { return c.find(dm, edited) != nil }) {
			t.Error("edit never came")
		} else if got := str(c.find(dm, edited), "content", "m.new_content", "body"); got != "answering this, edited" {
			t.Errorf("edit text %q", got)
		}
		like := func(e map[string]any) bool {
			return e["type"] == "m.reaction" && strings.TrimSuffix(str(e, "content", "m.relates_to", "key"), "\ufe0f") == "👍" && e["sender"] == ghost(fakegraph.Bea)
		}
		if !until(wait, func() bool { return c.find(dm, like) != nil }) {
			t.Error("reaction never came")
		}
		fake.React(fakegraph.ChatDM, homeworkTeams, fakegraph.Bea, "like", true)
		fake.Delete(fakegraph.ChatDM, replyTeams)
		redacted := func(target any) func(map[string]any) bool {
			return func(e map[string]any) bool {
				return e["type"] == "m.room.redaction" && (e["redacts"] == target || str(e, "content", "redacts") == target)
			}
		}
		if !until(wait, func() bool { return c.find(dm, redacted(reply["event_id"])) != nil }) {
			t.Error("deletion never came")
		}
		if r := c.find(dm, like); r == nil || !until(wait, func() bool { return c.find(dm, redacted(r["event_id"])) != nil }) {
			t.Error("removed reaction stayed")
		}
		fake.System(fakegraph.ChatDM, map[string]any{
			"@odata.type": "#microsoft.graph.callEndedEventMessageDetail", "callEventType": "call", "callDuration": "PT3M12S",
			"initiator": map[string]any{"user": map[string]any{"id": fakegraph.Bea, "displayName": "Bea Novak"}},
		})
		if !until(wait, func() bool { return c.find(dm, isBody("Call (3:12)")) != nil }) {
			t.Fatal("call log never came")
		}
		call := c.find(dm, isBody("Call (3:12)"))
		if str(call, "content", "com.beeper.action_message", "type") != "call" || call["sender"] != ghost(fakegraph.Bea) {
			t.Errorf("call log: %v", call)
		}
	})

	t.Run("old chat comes back", func(t *testing.T) {
		fake.Say(fakegraph.ChatOld, fakegraph.Cyril, "<p>back again</p>", nil)
		room := ""
		if !until(wait, func() bool { room = c.roomWhere(isBody("back again")); return room != "" }) {
			t.Fatal("never came")
		}
		if c.find(room, isBody("long ago")) != nil {
			t.Error("old history was bridged with it")
		}
	})

	// sync waits until the bridge has polled the DM once more after a write, for checks that something did not happen.
	sync := func(t *testing.T, tag string) {
		fake.Say(fakegraph.ChatDM, fakegraph.Bea, "<p>sentinel "+tag+"</p>", nil)
		if !until(wait, func() bool { return c.find(dm, isBody("sentinel "+tag)) != nil }) {
			t.Fatalf("sentinel %s never came", tag)
		}
	}

	t.Run("outgoing text, mention, reply", func(t *testing.T) {
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "hello from matrix"})
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "Hi Bea Novak", "format": "org.matrix.custom.html",
			"formatted_body": `Hi <a href="https://matrix.to/#/` + ghost(fakegraph.Bea) + `">Bea Novak</a>`})
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "replying",
			"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": homework["event_id"]}}})
		if !until(wait, func() bool { return len(e.writes("POST", "/chats/"+fakegraph.ChatDM+"/messages")) >= 3 }) {
			t.Fatalf("sent: %v", e.writes("POST", "/messages"))
		}
		sent := e.writes("POST", "/chats/"+fakegraph.ChatDM+"/messages")
		if content := str(sent[0].Body, "body", "content"); content != "hello from matrix" {
			t.Errorf("text: %q", content)
		}
		ms, _ := sent[1].Body["mentions"].([]any)
		if len(ms) != 1 || str(ms[0], "mentioned", "user", "id") != fakegraph.Bea || !strings.Contains(str(sent[1].Body, "body", "content"), `<at id="0">Bea Novak</at>`) {
			t.Errorf("mention: %v", sent[1].Body)
		}
		atts, _ := sent[2].Body["attachments"].([]any)
		if len(atts) != 1 || str(atts[0], "contentType") != "messageReference" || str(atts[0], "id") != homeworkTeams {
			t.Errorf("reply: %v", sent[2].Body)
		}
	})

	t.Run("outgoing photo and large file", func(t *testing.T) {
		img := c.upload(fakegraph.PNG, "dot.png", "image/png")
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.image", "body": "dot.png", "url": img, "info": map[string]any{"mimetype": "image/png"}})
		big := bytes.Repeat([]byte("0123456789abcdef"), 6<<16) // 6 MiB: goes through an upload session
		file := c.upload(big, "notes.bin", "application/octet-stream")
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.file", "body": "notes.bin", "url": file, "info": map[string]any{"mimetype": "application/octet-stream"}})
		if !until(wait, func() bool { return len(e.writes("POST", "/chats/"+fakegraph.ChatDM+"/messages")) >= 5 }) {
			t.Fatal("files never sent")
		}
		sent := e.writes("POST", "/chats/"+fakegraph.ChatDM+"/messages")
		if hosted, _ := sent[3].Body["hostedContents"].([]any); len(hosted) != 1 || !strings.Contains(str(sent[3].Body, "body", "content"), "../hostedContents/1/$value") {
			t.Errorf("photo: %v", sent[3].Body["body"])
		}
		atts, _ := sent[4].Body["attachments"].([]any)
		if len(atts) != 1 || str(atts[0], "contentType") != "reference" || str(atts[0], "name") != "notes.bin" || str(atts[0], "id") != "1a2b3c4d-1111-2222-3333-444455556666" {
			t.Errorf("file attachment: %v", sent[4].Body)
		}
		parts := e.writes("UPLOAD", "")
		if len(parts) < 2 || parts[0].Grant != "" {
			t.Errorf("upload session: %d pieces, token sent to the upload URL: %v", len(parts), len(parts) > 0 && parts[0].Grant != "")
		}
		if len(e.writes("POST", "/invite")) == 0 {
			t.Error("the chat's people were not given access to the file")
		}
	})

	t.Run("outgoing edit, reaction, deletion without echo", func(t *testing.T) {
		mine := c.find(dm, isBody("hello from matrix"))
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "* hello again",
			"m.new_content": map[string]any{"msgtype": "m.text", "body": "hello again"},
			"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": mine["event_id"]}})
		reaction := c.send(dm, "m.reaction", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": homework["event_id"], "key": "👍"}})
		if !until(wait, func() bool {
			return len(e.writes("PATCH", "/messages/")) == 1 && len(e.writes("POST", "/setReaction")) == 1
		}) {
			t.Fatalf("edit or reaction never sent: %v", e.fake.Log())
		}
		if got := str(e.writes("PATCH", "/messages/")[0].Body, "body", "content"); got != "hello again" {
			t.Errorf("edit: %q", got)
		}
		if got := str(e.writes("POST", "/setReaction")[0].Body, "reactionType"); got != "like" {
			t.Errorf("reaction type %q", got)
		}
		sync(t, "1")
		replaces := 0
		for _, ev := range c.roomEvents(dm) {
			if str(ev, "content", "m.relates_to", "event_id") == mine["event_id"] && str(ev, "content", "m.relates_to", "rel_type") == "m.replace" {
				replaces++
			}
		}
		likes := 0
		for _, ev := range c.roomEvents(dm) {
			if ev["type"] == "m.reaction" && str(ev, "content", "m.relates_to", "event_id") == homework["event_id"] {
				likes++
			}
		}
		if replaces != 1 || likes != 2 { // Bea's earlier, removed one and ours
			t.Errorf("echo: %d edits, %d reactions", replaces, likes)
		}
		_ = c.call("PUT", "/_matrix/client/v3/rooms/"+dm+"/redact/"+reaction+"/r1", map[string]any{}, nil)
		_ = c.call("PUT", "/_matrix/client/v3/rooms/"+dm+"/redact/"+mine["event_id"].(string)+"/r2", map[string]any{}, nil)
		if !until(wait, func() bool {
			return len(e.writes("POST", "/unsetReaction")) == 1 && len(e.writes("POST", "/softDelete")) == 1
		}) {
			t.Fatal("unreact or delete never sent")
		}
		if p := e.writes("POST", "/softDelete")[0].Path; !strings.HasPrefix(p, "/users/"+fakegraph.Me+"/chats/"+fakegraph.ChatDM+"/messages/") {
			t.Errorf("soft delete path %s", p)
		}
	})

	t.Run("read only at the newest message", func(t *testing.T) {
		_ = c.call("POST", "/_matrix/client/v3/rooms/"+dm+"/receipt/m.read/"+homework["event_id"].(string), map[string]any{}, nil)
		sync(t, "2")
		if n := len(e.writes("POST", "/markChatReadForUser")); n != 0 {
			t.Errorf("an old message marked the chat read (%d)", n)
		}
		newest := c.find(dm, isBody("sentinel 2"))
		_ = c.call("POST", "/_matrix/client/v3/rooms/"+dm+"/receipt/m.read/"+newest["event_id"].(string), map[string]any{}, nil)
		if !until(wait, func() bool { return len(e.writes("POST", "/markChatReadForUser")) == 1 }) {
			t.Error("reading the newest message did not mark the chat read")
		}
	})

	t.Run("directory search and new DM", func(t *testing.T) {
		var found struct {
			Results []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"results"`
		}
		if err := e.provision("POST", "/v3/search_users", map[string]string{"query": "Dana"}, &found); err != nil || len(found.Results) != 1 || found.Results[0].ID != fakegraph.Dana {
			t.Fatalf("search: %v %+v", err, found)
		}
		var dmResp struct {
			Room string `json:"dm_room_mxid"`
		}
		if err := e.provision("POST", "/v3/create_dm/dana@school.test", map[string]any{}, &dmResp); err != nil || dmResp.Room == "" {
			t.Fatalf("create DM: %v %+v", err, dmResp)
		}
		if len(e.writes("POST", "/chats")) == 0 {
			t.Error("no chat created in Teams")
		}
	})

	t.Run("token refresh and expired sign-in", func(t *testing.T) {
		refreshes := 0
		for _, entry := range fake.Log() {
			if entry.Method == "TOKEN" && entry.Grant == "refresh_token" {
				refreshes++
			}
		}
		if refreshes < 3 {
			t.Errorf("only %d refreshes", refreshes)
		}
		fake.Revoke()
		var who struct {
			Logins []struct {
				State struct {
					StateEvent string `json:"state_event"`
				} `json:"state"`
			} `json:"logins"`
		}
		ok := until(wait, func() bool {
			return e.provision("GET", "/v3/whoami", nil, &who) == nil && len(who.Logins) == 1 && who.Logins[0].State.StateEvent == "BAD_CREDENTIALS"
		})
		if !ok {
			t.Errorf("state after revoking: %+v", who)
		}
	})
}
