package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/slaghuis/telegram-mcp/internal/session"
)

type Bot struct {
	Token          string
	ChatID         int64
	Allowed        map[int64]bool
	HTTP           *http.Client
	Hub            *session.Hub
	Log            *slog.Logger
	lastUpdateID   int64
}

func New(token string, chatID int64, allowed []int64, hub *session.Hub, log *slog.Logger) *Bot {
	m := make(map[int64]bool, len(allowed))
	for _, u := range allowed {
		m[u] = true
	}
	return &Bot{
		Token: token, ChatID: chatID, Allowed: m, Hub: hub, Log: log,
		HTTP: &http.Client{Timeout: 70 * time.Second}, // > long-poll timeout
	}
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

func (b *Bot) call(ctx context.Context, method string, params url.Values, result any) error {
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/%s", b.Token, method)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint,
		strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return fmt.Errorf("decode: %w (body=%s)", err, string(body))
	}
	if !ar.OK {
		return fmt.Errorf("telegram: %s", ar.Description)
	}
	if result != nil {
		return json.Unmarshal(ar.Result, result)
	}
	return nil
}

type sentMessage struct {
	MessageID int64 `json:"message_id"`
}

type inlineKeyboard struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}

type inlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

func (b *Bot) SendMessage(ctx context.Context, text string, buttons []string, sessionID string) (int64, error) {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(b.ChatID, 10))
	params.Set("text", text)
	params.Set("parse_mode", "Markdown")
	if len(buttons) > 0 {
		kb := inlineKeyboard{InlineKeyboard: layoutButtons(buttons, sessionID)}
		b, _ := json.Marshal(kb)
		params.Set("reply_markup", string(b))
	}
	var sm sentMessage
	if err := b.call(ctx, "sendMessage", params, &sm); err != nil {
		return 0, err
	}
	return sm.MessageID, nil
}

func layoutButtons(opts []string, sessionID string) [][]inlineButton {
	// Try to lay out in rows of 2; data = "<sessionID>|<choice>"
	rows := make([][]inlineButton, 0, (len(opts)+1)/2)
	var row []inlineButton
	for i, o := range opts {
		row = append(row, inlineButton{
			Text:         o,
			CallbackData: fmt.Sprintf("%s|%s", sessionID, o),
		})
		if (i+1)%2 == 0 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

func (b *Bot) EditMessageDone(ctx context.Context, messageID int64, suffix string) error {
	// Removes keyboard and appends a status line.
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(b.ChatID, 10))
	params.Set("message_id", strconv.FormatInt(messageID, 10))
	params.Set("text", suffix)
	params.Set("parse_mode", "Markdown")
	// Clear inline keyboard
	params.Set("reply_markup", `{"inline_keyboard":[]}`)
	return b.call(ctx, "editMessageText", params, nil)
}

// ---- Long-poll ----

type update struct {
	UpdateID       int64             `json:"update_id"`
	Message        *tgMessage        `json:"message,omitempty"`
	CallbackQuery  *tgCallbackQuery  `json:"callback_query,omitempty"`
}

type tgMessage struct {
	MessageID int64   `json:"message_id"`
	From      tgUser  `json:"from"`
	Chat      tgChat  `json:"chat"`
	Text      string  `json:"text"`
	ReplyTo   *tgMessage `json:"reply_to_message,omitempty"`
}

type tgCallbackQuery struct {
	ID      string      `json:"id"`
	From    tgUser      `json:"from"`
	Data    string      `json:"data"`
	Message *tgMessage  `json:"message"`
}

type tgUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

func (b *Bot) RunPolling(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		params := url.Values{}
		params.Set("timeout", "60")
		params.Set("offset", strconv.FormatInt(b.lastUpdateID+1, 10))
		params.Set("allowed_updates", `["message","callback_query"]`)

		var updates []update
		if err := b.call(ctx, "getUpdates", params, &updates); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			b.Log.Warn("getUpdates", "err", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			if u.UpdateID > b.lastUpdateID {
				b.lastUpdateID = u.UpdateID
			}
			b.handleUpdate(ctx, u)
		}
	}
}

func (b *Bot) handleUpdate(ctx context.Context, u update) {
	switch {
	case u.CallbackQuery != nil:
		b.handleCallback(ctx, u.CallbackQuery)
	case u.Message != nil:
		b.handleMessage(ctx, u.Message)
	}
}

func (b *Bot) handleCallback(ctx context.Context, cq *tgCallbackQuery) {
	if len(b.Allowed) > 0 && !b.Allowed[cq.From.ID] {
		b.Log.Warn("unauthorized callback", "user_id", cq.From.ID)
		b.answerCallback(ctx, cq.ID, "unauthorized")
		return
	}
	parts := strings.SplitN(cq.Data, "|", 2)
	if len(parts) != 2 {
		b.answerCallback(ctx, cq.ID, "bad data")
		return
	}
	sessionID, choice := parts[0], parts[1]

	sess, ok := b.Hub.Get(sessionID)
	if !ok {
		// Not in memory. Could be already-resolved OR a very stale message.
		b.answerCallback(ctx, cq.ID, "no longer active")
		return
	}

	reply := session.Reply{Choice: choice, Status: session.StatusAnswered}
	_ = b.Hub.Resolve(sessionID, reply)
	b.answerCallback(ctx, cq.ID, "✓ "+choice)

	// Different label for orphans so the human knows context changed.
	var footer string
	if sess.Orphaned {
		footer = fmt.Sprintf("♻️ *Answered post-restart:* `%s` by @%s", choice, cq.From.Username)
	} else {
		footer = fmt.Sprintf("✅ *Answered:* `%s` by @%s", choice, cq.From.Username)
	}
	_ = b.EditMessageDone(ctx, sess.MessageID,
		fmt.Sprintf("%s\n\n%s", sess.Prompt, footer))
}

func (b *Bot) handleMessage(ctx context.Context, m *tgMessage) {
	if len(b.Allowed) > 0 && !b.Allowed[m.From.ID] {
		return
	}
	// Only free-text replies to our bot messages resolve ask_question sessions.
	if m.ReplyTo == nil {
		b.handleCommand(ctx, m)
		return
	}
	sess, ok := b.Hub.GetByMessage(m.ReplyTo.MessageID)
	if !ok || sess.Kind != session.KindAsk {
		return
	}
	_ = b.Hub.Resolve(sess.ID, session.Reply{
		Text: m.Text, Status: session.StatusAnswered,
	})
	_ = b.EditMessageDone(ctx, sess.MessageID,
		fmt.Sprintf("%s\n\n💬 *Answered* by @%s", sess.Prompt, m.From.Username))
}

func (b *Bot) handleCommand(ctx context.Context, m *tgMessage) {
	switch strings.ToLower(strings.TrimSpace(m.Text)) {
	case "/pending", "pending":
		pend := b.Hub.ListPending()
		if len(pend) == 0 {
			_, _ = b.SendMessage(ctx, "No pending prompts.", nil, "")
			return
		}
		var buf bytes.Buffer
		buf.WriteString("*Pending prompts:*\n")
		for _, s := range pend {
			fmt.Fprintf(&buf, "• `%s` (%s) — %s\n", s.ID[:8], s.Kind,
				truncate(s.Prompt, 60))
		}
		_, _ = b.SendMessage(ctx, buf.String(), nil, "")
	case "/help", "help":
		_, _ = b.SendMessage(ctx,
			"Commands:\n• `/pending` — list open prompts\n"+
				"• reply to any question message to answer it", nil, "")
	}
}

func (b *Bot) answerCallback(ctx context.Context, id, text string) {
	params := url.Values{}
	params.Set("callback_query_id", id)
	if text != "" {
		params.Set("text", text)
	}
	_ = b.call(ctx, "answerCallbackQuery", params, nil)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
