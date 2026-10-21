package feishu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// These tests cover the thread_bootstrap_quote switch: when false, the first
// message in a pre-existing thread is dispatched WITHOUT the parent/root reply
// chain prepended to ExtraContent. The switch only gates the bootstrap branch --
// ordinary quoted replies and non-thread sessions keep their quote.
//
// Every case drives the real production entry point (onMessage) rather than
// dispatchMessage directly, so the thread sessionKey derivation and
// markThreadSessionActive bootstrap flag are exercised too.

const (
	bsqRootMsgID   = "om_root_bsq"
	bsqParentMsgID = "om_parent_bsq"
	// bsqOldText is the text of the pre-existing message that would be pulled
	// into the agent context via the reply chain.
	bsqOldText = "PREVIOUS_TOPIC_TEXT_MARKER"
	// bsqNewText is the user's actual new message body (after @bot stripping).
	bsqNewText = "help me with this"
)

// bsqMock starts a Feishu API mock that serves any parent/root message so the
// reply chain resolves to bsqOldText. parentFetches counts how many times such
// a message was retrieved -- the server-side ground truth for "was the chain
// pulled in or not".
//
// Any message id is served, not just one: in the bootstrap path parentID falls
// back to RootId, while an explicit reply carries ParentId, so the two cases hit
// different message ids.
func bsqMock(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var parentFetches atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "success", "expire": 7200,
				"tenant_access_token": "tenant-token",
			})
		case bsqIsMessageFetch(r.URL.Path):
			parentFetches.Add(1)
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{
					"items": []map[string]any{
						{
							"msg_type":  "text",
							"parent_id": "",
							"sender":    map[string]any{"id": "ou_old", "sender_type": "user"},
							"body":      map[string]any{"content": `{"text":"` + bsqOldText + `"}`},
						},
					},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/open-apis/contact/v3/users/"):
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{"code": 0, "msg": "success",
				"data": map[string]any{"user": map[string]any{"name": "Alice"}}})
		default:
			// user/chat info lookups and anything else: succeed with empty data
			// so dispatch proceeds past optional metadata fetches.
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{"code": 0, "msg": "success", "data": map[string]any{}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &parentFetches
}

// bsqIsMessageFetch reports whether the path is a single-message fetch
// (/open-apis/im/v1/messages/<id>) rather than a sub-resource such as
// /messages/<id>/resources/<key>.
func bsqIsMessageFetch(path string) bool {
	const prefix = "/open-apis/im/v1/messages/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

// bsqBuildEvent builds a Feishu message-receive event. parentID populates
// ParentId (an explicit reply); rootID populates RootId (thread root).
func bsqBuildEvent(msgID, msgType, content, chatID, parentID, rootID string, mentions []*larkim.MentionEvent) *larkim.P2MessageReceiveV1 {
	chatType := "group"
	senderType := "user"
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	ev := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId:   &larkim.UserId{OpenId: stringPtr("ou_user")},
				SenderType: &senderType,
			},
			Message: &larkim.EventMessage{
				MessageId:   stringPtr(msgID),
				ChatId:      stringPtr(chatID),
				ChatType:    &chatType,
				MessageType: stringPtr(msgType),
				Content:     stringPtr(content),
				CreateTime:  &now,
				Mentions:    mentions,
			},
		},
	}
	if parentID != "" {
		ev.Event.Message.ParentId = stringPtr(parentID)
	}
	if rootID != "" {
		ev.Event.Message.RootId = stringPtr(rootID)
	}
	return ev
}

func bsqBotMention() []*larkim.MentionEvent {
	return []*larkim.MentionEvent{
		{Key: stringPtr("@_user_1"), Id: &larkim.UserId{OpenId: stringPtr("ou_bot")}, Name: stringPtr("claude code")},
	}
}

// TestThreadBootstrapQuoteDisabledStillProcessesFirstMessage is the core
// guarantee: turning the switch off must NOT suppress the first message in a
// thread. It is still dispatched, with Content intact and NO old topic text in
// ExtraContent.
func TestThreadBootstrapQuoteDisabledStillProcessesFirstMessage(t *testing.T) {
	srv, parentFetches := bsqMock(t)

	received := make(chan *core.Message, 8)
	p := &Platform{
		platformName:         "feishu",
		domain:               srv.URL,
		appID:                "cli_bsq_off",
		appSecret:            "secret-bsq-off",
		botOpenID:            "ou_bot",
		threadIsolation:      true,
		threadBootstrapQuote: false,
		dedup:                &core.MessageDedup{},
		client: lark.NewClient("cli_bsq_off", "secret-bsq-off",
			lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
		handler: func(_ core.Platform, msg *core.Message) { received <- msg },
	}

	// First message in a pre-existing thread: thread key is fresh, so
	// markThreadSessionActive reports bootstrap and parentID falls back to
	// RootId. That is exactly the bootstrap branch under test.
	ev := bsqBuildEvent("om_first", "text", `{"text":"@_user_1 `+bsqNewText+`"}`,
		"oc_bsq", "", bsqRootMsgID, bsqBotMention())

	if err := p.onMessage(context.Background(), ev); err != nil {
		t.Fatalf("onMessage() error = %v", err)
	}

	select {
	case msg := <-received:
		if msg.Content != bsqNewText {
			t.Fatalf("Content = %q, want %q (first message must still be processed)", msg.Content, bsqNewText)
		}
		if strings.Contains(msg.ExtraContent, bsqOldText) {
			t.Fatalf("ExtraContent contains old topic text %q with thread_bootstrap_quote=false:\n%s",
				bsqOldText, msg.ExtraContent)
		}
		if msg.ExtraContent != "" {
			t.Errorf("ExtraContent = %q, want empty when bootstrap quote is disabled", msg.ExtraContent)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for dispatched message -- first message was NOT processed")
	}

	if n := parentFetches.Load(); n != 0 {
		t.Errorf("parent message fetched %d time(s), want 0 -- reply chain should not be pulled", n)
	}
}

// TestThreadBootstrapQuoteDefaultsOn pins the default: with no
// thread_bootstrap_quote in the config, the parent/root chain IS still injected.
// Built through newPlatform so the real config-parsing default is exercised
// (a hand-built &Platform{} would have the zero value false, not true).
func TestThreadBootstrapQuoteDefaultsOn(t *testing.T) {
	srv, parentFetches := bsqMock(t)

	received := make(chan *core.Message, 8)
	pf, err := newPlatform("feishu", srv.URL, map[string]any{
		"app_id":           "cli_bsq_default",
		"app_secret":       "secret-bsq-default",
		"thread_isolation": true,
		// thread_bootstrap_quote intentionally absent -> must default to true
	})
	if err != nil {
		t.Fatalf("newPlatform error: %v", err)
	}
	fp := extractBasePlatform(pf)
	if fp == nil {
		t.Fatal("expected *Platform or *interactivePlatform")
	}
	if !fp.threadBootstrapQuote {
		t.Fatal("thread_bootstrap_quote should default to true when unset")
	}
	fp.botOpenID = "ou_bot"
	fp.handler = func(_ core.Platform, msg *core.Message) { received <- msg }

	ev := bsqBuildEvent("om_first_default", "text", `{"text":"@_user_1 `+bsqNewText+`"}`,
		"oc_bsq", "", bsqRootMsgID, bsqBotMention())
	if err := fp.onMessage(context.Background(), ev); err != nil {
		t.Fatalf("onMessage() error = %v", err)
	}

	select {
	case msg := <-received:
		if !strings.Contains(msg.ExtraContent, bsqOldText) {
			t.Fatalf("ExtraContent = %q, want it to contain old topic text %q by default",
				msg.ExtraContent, bsqOldText)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for dispatched message")
	}

	if n := parentFetches.Load(); n == 0 {
		t.Error("parent message was never fetched -- default should still bootstrap the reply chain")
	}
}

// TestThreadBootstrapQuoteDisabledKeepsOrdinaryQuote proves the switch only
// gates the bootstrap branch. With thread_isolation=false an explicit user reply
// (ParentId set) must still pull in the quoted message.
func TestThreadBootstrapQuoteDisabledKeepsOrdinaryQuote(t *testing.T) {
	srv, parentFetches := bsqMock(t)

	received := make(chan *core.Message, 8)
	p := &Platform{
		platformName:         "feishu",
		domain:               srv.URL,
		appID:                "cli_bsq_reply",
		appSecret:            "secret-bsq-reply",
		botOpenID:            "ou_bot",
		threadIsolation:      false,
		threadBootstrapQuote: false,
		dedup:                &core.MessageDedup{},
		client: lark.NewClient("cli_bsq_reply", "secret-bsq-reply",
			lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
		handler: func(_ core.Platform, msg *core.Message) { received <- msg },
	}

	ev := bsqBuildEvent("om_reply", "text", `{"text":"@_user_1 `+bsqNewText+`"}`,
		"oc_bsq", bsqParentMsgID, "", bsqBotMention())

	if err := p.onMessage(context.Background(), ev); err != nil {
		t.Fatalf("onMessage() error = %v", err)
	}

	select {
	case msg := <-received:
		if !strings.Contains(msg.ExtraContent, bsqOldText) {
			t.Fatalf("ExtraContent = %q, want the quoted text %q -- an ordinary explicit reply must still inject the quote",
				msg.ExtraContent, bsqOldText)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for dispatched message")
	}

	if n := parentFetches.Load(); n == 0 {
		t.Error("parent message was never fetched -- ordinary reply must still resolve the quote")
	}
}

// TestThreadBootstrapQuoteDisabledDropsBareMention pins the one visible
// behaviour change of the switch: a bare "@bot" with no body used to survive
// the empty-message check only because the bootstrap quote supplied text. With
// the quote gone there is nothing left, so it is dropped by the empty-message
// rule in dispatchMessageWithHistory (text == "" && historyText == "" &&
// quoted.text == "" ...). This is intentional, not a regression.
func TestThreadBootstrapQuoteDisabledDropsBareMention(t *testing.T) {
	srv, parentFetches := bsqMock(t)

	received := make(chan *core.Message, 8)
	p := &Platform{
		platformName:         "feishu",
		domain:               srv.URL,
		appID:                "cli_bsq_bare",
		appSecret:            "secret-bsq-bare",
		botOpenID:            "ou_bot",
		threadIsolation:      true,
		threadBootstrapQuote: false,
		dedup:                &core.MessageDedup{},
		client: lark.NewClient("cli_bsq_bare", "secret-bsq-bare",
			lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
		handler: func(_ core.Platform, msg *core.Message) { received <- msg },
	}

	// Mention only -- stripMentions leaves an empty string.
	ev := bsqBuildEvent("om_bare", "text", `{"text":"@_user_1"}`,
		"oc_bsq", "", bsqRootMsgID, bsqBotMention())

	if err := p.onMessage(context.Background(), ev); err != nil {
		t.Fatalf("onMessage() error = %v", err)
	}

	select {
	case msg := <-received:
		t.Fatalf("bare @bot with no body should be dropped by the empty-message rule, but handler received %q (ExtraContent=%q)",
			msg.MessageID, msg.ExtraContent)
	case <-time.After(500 * time.Millisecond):
		// expected: nothing dispatched
	}

	if n := parentFetches.Load(); n != 0 {
		t.Errorf("parent message fetched %d time(s), want 0", n)
	}
}
