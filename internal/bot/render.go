package bot

import (
	"fmt"
	"strings"
)

// FormatApproval produces the Telegram message body for an approval request.
func FormatApproval(prompt, context, taskTag string) string {
	var b strings.Builder
	b.WriteString("🤖 *Approval requested*\n\n")
	if taskTag != "" {
		fmt.Fprintf(&b, "`tag: %s`\n\n", escape(taskTag))
	}
	b.WriteString("*Decision:*\n")
	b.WriteString(escape(prompt))
	if context != "" {
		b.WriteString("\n\n*Context:*\n")
		b.WriteString("```\n")
		b.WriteString(truncateCodeBlock(context, 1200))
		b.WriteString("\n```")
	}
	return b.String()
}

func FormatQuestion(prompt, taskTag string) string {
	var b strings.Builder
	b.WriteString("❓ *Question from agent*\n\n")
	if taskTag != "" {
		fmt.Fprintf(&b, "`tag: %s`\n\n", escape(taskTag))
	}
	b.WriteString(escape(prompt))
	b.WriteString("\n\n_(Reply to this message to answer)_")
	return b.String()
}

func FormatNotify(message, taskTag string) string {
	if taskTag == "" {
		return "📣 " + escape(message)
	}
	return fmt.Sprintf("📣 `[%s]` %s", escape(taskTag), escape(message))
}

// Telegram Markdown is picky; this is intentionally minimal.
func escape(s string) string {
	// We're using classic Markdown; strip problematic characters.
	s = strings.ReplaceAll(s, "`", "'")
	return s
}

func truncateCodeBlock(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}