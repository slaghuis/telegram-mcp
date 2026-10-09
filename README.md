 # Telegram MCP Server
A Go-based MCP server that lets agents notify you, ask you questions, and request approvals via Telegram — then blocks until you reply. This is what turns "autonomous agent" into "autonomous agent with a human in the loop on the phone."

 ## Architecture 
```
┌─────────────────────────────────────────────────────────────────┐
│  Agent (opencode / Claude Code / Cursor)                        │
│    tool: telegram.request_approval(context, options)            │
└─────────────────────────────────────────────────────────────────┘
                            │ stdio (JSON-RPC)
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│  Telegram MCP Server (this)                                     │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  MCP tool handlers                                        │  │
│  │   notify / ask_question / request_approval / list_pending │  │
│  └───────────────────────────────────────────────────────────┘  │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  Session Hub (in-process)                                 │  │
│  │   - pending map[sessionID] → chan Reply                   │  │
│  │   - timeouts, cancellation                                │  │
│  └───────────────────────────────────────────────────────────┘  │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  Telegram Bot (long-poll goroutine)                       │  │
│  │   - sends messages with inline buttons                    │  │
│  │   - receives callbacks + text replies                     │  │
│  │   - persists session state in SQLite (survive restart)    │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                            │ HTTPS long-poll
                            ▼
                     Telegram Bot API
                            │
                            ▼
                       Your phone 📱
```
Key design points:
 - **One daemon, many agents**. The server runs as a long-lived process (not spawned per-session). Multiple agents share it over MCP's stdio... which creates a problem: stdio implies one subprocess per client. Solution: run it as an HTTP-SSE MCP server, which opencode and claude-code both support. All agents on your machine share one Telegram bot and one session hub.
 - **Blocking tools**. request_approval doesn't return until you tap a button or it times out. The agent literally pauses mid-task. This is the whole point.
 - **Persistent sessions**. SQLite stores pending approvals so a server restart doesn't orphan the agent — on reconnect, the agent picks up the reply.
 - **Multiple session shapes**. notify (fire-and-forget), ask_question (free-text reply), request_approval (button choice).

 ## Create the Bot & Get Chat ID
 - In Telegram, message `@BotFather` → `/newbot` → name it `my-sdlc-bot` → save the **bot token**.
 - Message your new bot `/start` from your personal account.
Get your `chat_id`:
```
curl -s https://api.telegram.org/bot<TOKEN>/getUpdates | jq '.result[0].message.chat.id'
```
Save both. We'll put them in config.

 ## Build & Run
```
cd telegram-mcp
go build -o ~/.local/bin/telegram-mcp ./cmd/telegram-mcp

export TELEGRAM_BOT_TOKEN=123456:ABC...
~/.local/bin/telegram-mcp -config ./config.yaml -transport sse
```
You should see:
```
starting SSE server listen=:8765
```
Hit the health:
```
curl -s http://localhost:8765/sse -I
```
Should return `200 OK` with `Content-Type: text/event-stream`.
 ## Run as a Background Service (launchd)
Create `~/Library/LaunchAgents/com.you.telegram-mcp.plist`:
```
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.you.telegram-mcp</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/you/.local/bin/telegram-mcp</string>
    <string>-config</string><string>/Users/you/telegram-mcp/config.yaml</string>
    <string>-transport</string><string>sse</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>TELEGRAM_BOT_TOKEN</key><string>123456:ABC...</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/you/.local/share/telegram-mcp.log</string>
  <key>StandardErrorPath</key><string>/Users/you/.local/share/telegram-mcp.err</string>
  <key>WorkingDirectory</key><string>/Users/you/telegram-mcp</string>
</dict>
</plist>
```
```
launchctl load ~/Library/LaunchAgents/com.you.telegram-mcp.plist
```

 ## Wiring Into Agents

 ### opencode
```
{
  "mcp": {
    "telegram": {
      "type": "remote",
      "url": "http://localhost:8765/sse",
      "enabled": true
    }
  }
}
```
 ### claudecode
```
claude mcp add --transport sse telegram http://localhost:8765/sse
```
 ### Cursor
```
{
  "mcpServers": {
    "telegram": {
      "url": "http://localhost:8765/sse"
    }
  }
}
```
Now every agent on your machine shares the same bot, hub, and session storage. No conflicts, one point of contact on your phone.

 ## Test End-to-End
Open `@modelcontextprotocol/inspector` pointed at your SSE server:

```
npx @modelcontextprotocol/inspector --transport sse http://localhost:8765/sse
```
Try:
 - **notify** with message "deployment started" → Telegram ping.
 - **request_approval** with prompt="Deploy v1.2.3 to prod?", options="Deploy,Hold,Rollback", context="12 commits, all tests green" → See buttons on your phone, tap one, tool call completes.
 - **ask_question** with prompt="Which region?" → Reply to the Telegram message with text; tool call returns your text.

 ## Rehydration Behaviour You'll See
 ### On restart with 2 pending approvals
```
INFO rehydrate complete loaded=2 expired_on_startup=0
INFO hub ready rehydrated=2 expired_on_startup=0
INFO starting SSE + pipeline HTTP server listen=:8765
```
Both messages on your phone are still tappable. If you tap:
```
♻️ Answered post-restart: Deploy by @slaghuis
```
The visual difference tells you this approval resolved after the server came back up, so you know an agent might not have received the reply directly.
 ### On restart with an overdue session
```
INFO rehydrate complete loaded=0 expired_on_startup=1
```
The row is marked timeout. If you tap the stale button, you get "no longer active" in Telegram and nothing else happens.

 ### Pipeline retry with stable session ID
Pipeline-lib's telegram.Client can now (optionally) provide a deterministic ID:
```sessionID := fmt.Sprintf("%s-%s-%s", cfg.Service, env, version) 
// e.g. "myservice-staging-v1.2.3"
```
Caller workflow:
 1. First POST /pipeline/approve with session_id=myservice-staging-v1.2.3 blocks.
 2. Server restarts; pipeline's HTTP call errors out.
 3. Pipeline retries the same POST. The in-memory session is gone, but the DB row exists as pending → new in-memory Session is created, joined by the retry. Same Telegram message, no duplicate.
 4. User taps. Both the original (dead) and retrying callers' DB row resolves.
 5. Alternatively, pipeline can GET /pipeline/approve/myservice-staging-v1.2.3 to poll without re-sending the message.
I'll leave the pipeline-lib update to use stable IDs as a one-line change you can decide on. The server supports it either way.

 ## Operational Notes
 - **Reconnection safety**: If the server restarts mid-approval, the SQLite row survives but the in-memory `replyCh` doesn't. The agent's tool call will error with "session not found" from Telegram's side when you tap. Mitigation: agents should retry blocking calls with idempotency keys, or you can extend the hub to rehydrate pending sessions on startup and resolve them when callbacks arrive (left as an easy extension — load `store.LoadPending()` in `main.go` after `NewHub` and re-register them).
 - **Rate limits**: Telegram allows ~30 messages/sec per bot. Not a concern for human-in-loop workflows, but if an agent goes crazy with notify, add a token bucket.
 - **Long-lived approvals**: The default 30-min timeout is for interactive work. For overnight runs, agents can pass `timeout_seconds: 28800` (8h). Telegram message buttons remain tappable until the message is edited.
 - **Multiple users**: Add more IDs to `allowed_user_ids`. The first responder wins — useful for team setups where any teammate can approve.
 - **Security**: The SSE endpoint has no auth. It's on localhost by default. If you expose it over Tailscale or a tunnel, put a reverse proxy with bearer-token auth in front.
 - **Markdown gotchas**: Telegram's classic Markdown breaks on unbalanced backticks or asterisks in `context`. The `render.go` escaping is minimal — if agents send arbitrary code, switch to `MarkdownV2` and escape properly, or send as `parse_mode=HTML`.

 ## Example Agent Behavior

With this wired in, an agent given "implement rate limiting on the login endpoint and deploy to staging" will typically:
 1. `search_code("login endpoint handler")` — MCP code search.
 2. `telegram_notify(message="Starting rate-limit implementation", task_tag="RL-001")`.
 3. Writes code, runs tests, commits.
 4. `telegram_request_approval(prompt="Merge PR #42 to main?", context="<diff summary + test results>", options="Merge,Hold,Review manually", task_tag="RL-001")`.
 5. You tap "Merge" on your phone. Agent continues.
 6. `telegram_request_approval(prompt="Deploy to staging?", context="commit abc1234", options="Deploy,Hold", task_tag="RL-001")`.
 7. You tap "Deploy". Deployment runs.
 8. `telegram_notify(message="✅ Deployed. Smoke tests passing.", task_tag="RL-001")`.

Total human interaction: three taps on your phone while you were in a meeting.

