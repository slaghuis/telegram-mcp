package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/slaghuis/telegram-mcp/internal/bot"
	"github.com/slaghuis/telegram-mcp/internal/metrics"
	"github.com/slaghuis/telegram-mcp/internal/session"
)

func RegisterApproval(s *server.MCPServer, b *bot.Bot, hub *session.Hub, defaultTO time.Duration) {
	tool := mcp.NewTool("telegram_request_approval",
		mcp.WithDescription(
			"Request a decision from the user with a fixed set of button options. "+
				"BLOCKS until the user taps a button or the timeout expires. "+
				"Use this before destructive operations (merging to main, deploying, "+
				"deleting data) or at architectural branch points."),
		mcp.WithString("prompt", mcp.Required(),
			mcp.Description("The decision prompt, e.g. 'Deploy v1.2.3 to production?'")),
		mcp.WithString("context",
			mcp.Description("Optional background context (diff summary, test results, etc.).")),
		mcp.WithString("options",
			mcp.Description("Comma-separated list of button labels. Default: 'Approve,Reject'.")),
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
		optsStr := req.GetString("options", "Approve,Reject")
		var options []string
		for _, o := range strings.Split(optsStr, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				options = append(options, o)
			}
		}
		if len(options) == 0 {
			options = []string{"Approve", "Reject"}
		}
		if len(options) > 8 {
			return mcp.NewToolResultError("too many options (max 8)"), nil
		}

		tag := req.GetString("task_tag", "")
		contextStr := req.GetString("context", "")

		timeout := defaultTO
		if to := req.GetFloat("timeout_seconds", 0); to > 0 {
			timeout = time.Duration(to) * time.Second
		}

		sess := &session.Session{
			ID:        uuid.NewString(),
			Kind:      session.KindApproval,
			Prompt:    prompt,
			Options:   options,
			Context:   contextStr,
			TaskTag:   tag,
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(timeout),
		}
		if err := hub.Register(sess); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		metrics.SessionsCreated.WithLabelValues(string(sess.Kind)).Inc()

		text := bot.FormatApproval(prompt, contextStr, tag)
		msgID, err := b.SendMessage(ctx, text, options, sess.ID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		hub.UpdateMessageID(sess.ID, msgID)

		reply := hub.Wait(ctx, sess)
		result := map[string]any{
			"status": string(reply.Status),
			"choice": reply.Choice,
		}
		b, _ := json.Marshal(result)
		if reply.Status != session.StatusAnswered {
			return mcp.NewToolResultError(fmt.Sprintf("no answer: %s", reply.Status)), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	})
}