package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/slaghuis/telegram-mcp/internal/bot"
	"github.com/slaghuis/telegram-mcp/internal/session"
)

func RegisterAsk(s *server.MCPServer, b *bot.Bot, hub *session.Hub, defaultTO time.Duration) {
	tool := mcp.NewTool("telegram_ask_question",
		mcp.WithDescription(
			"Ask the user an open-ended question and WAIT for a free-text reply. "+
				"BLOCKS until the user replies via Telegram (by replying to the message) "+
				"or the timeout expires. Use for clarifications or decisions that "+
				"don't fit a fixed set of options."),
		mcp.WithString("prompt", mcp.Required(),
			mcp.Description("The question to ask the user.")),
		mcp.WithString("task_tag",
			mcp.Description("Optional tag identifying the task.")),
		mcp.WithNumber("timeout_seconds",
			mcp.Description("Timeout in seconds (default: configured default).")),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		prompt, err := req.RequireString("prompt")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		tag := req.GetString("task_tag", "")
		timeout := defaultTO
		if to := req.GetFloat("timeout_seconds", 0); to > 0 {
			timeout = time.Duration(to) * time.Second
		}

		sess := &session.Session{
			ID:        uuid.NewString(),
			Kind:      session.KindAsk,
			Prompt:    prompt,
			TaskTag:   tag,
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(timeout),
		}
		if err := hub.Register(sess); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		text := bot.FormatQuestion(prompt, tag)
		msgID, err := b.SendMessage(ctx, text, nil, sess.ID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		hub.UpdateMessageID(sess.ID, msgID)

		reply := hub.Wait(ctx, sess)
		result := map[string]any{
			"status": string(reply.Status),
			"answer": reply.Text,
		}
		b, _ := json.Marshal(result)
		if reply.Status != session.StatusAnswered {
			return mcp.NewToolResultError(fmt.Sprintf("no answer: %s", reply.Status)), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	})
}