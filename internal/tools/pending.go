package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/slaghuis/telegram-mcp/internal/session"
)

func RegisterPending(s *server.MCPServer, hub *session.Hub) {
	tool := mcp.NewTool("telegram_list_pending",
		mcp.WithDescription(
			"List currently pending Telegram prompts (approvals and questions "+
				"that haven't been answered yet). Useful for the agent to check "+
				"if it has unresolved requests before issuing new ones."),
	)
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		list := hub.ListPending()
		type pub struct {
			ID        string    `json:"id"`
			Kind      string    `json:"kind"`
			Prompt    string    `json:"prompt"`
			Options   []string  `json:"options,omitempty"`
			TaskTag   string    `json:"task_tag,omitempty"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		out := make([]pub, 0, len(list))
		for _, s := range list {
			out = append(out, pub{
				ID: s.ID, Kind: string(s.Kind), Prompt: s.Prompt,
				Options: s.Options, TaskTag: s.TaskTag,
				ExpiresAt: s.ExpiresAt,
			})
		}
		b, _ := json.Marshal(map[string]any{"count": len(out), "pending": out})
		return mcp.NewToolResultText(string(b)), nil
	})
}