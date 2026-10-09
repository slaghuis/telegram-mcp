package tools

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/slaghuis/telegram-mcp/internal/bot"
)

type Deps struct {
	Bot     *bot.Bot
	Hub     interface { // decoupled for testing
		Register(s any) error
	}
	DefaultTimeout time.Duration
}

func RegisterNotify(s *server.MCPServer, b *bot.Bot) {
	tool := mcp.NewTool("telegram_notify",
		mcp.WithDescription(
			"Send a non-blocking status update to the user via Telegram. "+
				"Use this for progress updates, warnings, or completion notices "+
				"that do NOT require a response. Returns immediately."),
		mcp.WithString("message", mcp.Required(),
			mcp.Description("The message to send. Supports plain text.")),
		mcp.WithString("task_tag",
			mcp.Description("Optional tag identifying which task/workflow this belongs to.")),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		msg, err := req.RequireString("message")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		tag := req.GetString("task_tag", "")
		text := bot.FormatNotify(msg, tag)
		if _, err := b.SendMessage(ctx, text, nil, ""); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText("sent"), nil
	})
}