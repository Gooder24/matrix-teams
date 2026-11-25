package connector

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestReactions(t *testing.T) {
	for emoji, want := range map[string]string{"👍": "like", "👍️": "like", "❤": "heart", "❤️": "heart", "♥": "heart", "😮": "surprised", "🎉": "🎉"} {
		if got := reactionType(emoji); got != want {
			t.Errorf("reactionType(%q) = %q, want %q", emoji, got, want)
		}
	}
	for kind, want := range map[string]string{"like": "👍", "heart": "❤️", "♥": "❤️", "🎉": "🎉"} {
		if got := reactionEmoji(kind); got != want {
			t.Errorf("reactionEmoji(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestCallLog(t *testing.T) {
	cases := map[string]string{"PT3M12S": "Call (3:12)", "PT1H2M3.5S": "Call (1:02:03)", "P1DT0H0M1S": "Call (24:00:01)", "": "Call", "PT0S": "Call"}
	for duration, want := range cases {
		detail, _ := json.Marshal(map[string]any{"@odata.type": "#microsoft.graph.callEndedEventMessageDetail", "callEventType": "call", "callDuration": duration})
		_, text, ok := callLog(&graphMessage{EventDetail: detail})
		if !ok || text != want {
			t.Errorf("%q: %q %v", duration, text, ok)
		}
	}
	added, _ := json.Marshal(map[string]any{"@odata.type": "#microsoft.graph.membersAddedEventMessageDetail"})
	if _, _, ok := callLog(&graphMessage{EventDetail: added}); ok {
		t.Error("a member note became a call")
	}
}

func TestLoginErrors(t *testing.T) {
	cases := map[[2]string]string{
		{"invalid_grant", "AADSTS65001: no consent"}:     "admin_consent",
		{"invalid_request", "AADSTS53003: blocked"}:      "conditional_access",
		{"access_denied", ""}:                            "denied",
		{"invalid_client", "AADSTS700016: no such app"}:  "bad_client",
		{"interaction_required", "AADSTS50076: mfa"}:     "interaction_required",
		{"invalid_grant", "AADSTS70008: expired"}:        "invalid_grant",
		{"temporarily_unavailable", "AADSTS90033: busy"}: "temporarily_unavailable",
	}
	for in, want := range cases {
		var le LoginError
		if err := loginError(in[0], in[1]); !errors.As(err, &le) || le.Code != want {
			t.Errorf("%v: %v", in, err)
		}
	}
}

func TestIDClaims(t *testing.T) {
	// header.payload.signature with {"tid":"t-1","oid":"O-2"}
	tid, oid := idClaims("eyJhbGciOiJub25lIn0.eyJ0aWQiOiJ0LTEiLCJvaWQiOiJPLTIifQ.x")
	if tid != "t-1" || oid != "O-2" {
		t.Errorf("%q %q", tid, oid)
	}
	if tid, oid = idClaims("garbage"); tid != "" || oid != "" {
		t.Error("garbage gave claims")
	}
}

func TestMessageID(t *testing.T) {
	chat, msg := parseMessageID(makeMessageID("19:abc@thread.v2", "1700000000000"))
	if chat != "19:abc@thread.v2" || msg != "1700000000000" {
		t.Errorf("%q %q", chat, msg)
	}
}

func TestKeptParts(t *testing.T) {
	body := `<p>old text</p><attachment id="abc"></attachment><img src="https://graph.test/v1.0/chats/x/messages/1/hostedContents/aW1n/$value" alt="pic"><img src="https://example.test/emoji.png">`
	want := `<attachment id="abc"></attachment><img src="https://graph.test/v1.0/chats/x/messages/1/hostedContents/aW1n/$value" alt="pic">`
	if got := keptParts(body); got != want {
		t.Errorf("keptParts = %q", got)
	}
}

func TestStampSortsAsText(t *testing.T) {
	a := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	b := a.Add(100 * time.Millisecond)
	if stamp(a) != "2026-11-25T10:00:00.000Z" || stamp(a) >= stamp(b) {
		t.Errorf("stamps %q and %q do not sort", stamp(a), stamp(b))
	}
}
