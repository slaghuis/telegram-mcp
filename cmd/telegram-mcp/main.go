package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/slaghuis/telegram-mcp/internal/bot"
	"github.com/slaghuis/telegram-mcp/internal/config"
	"github.com/slaghuis/telegram-mcp/internal/metrics"
	"github.com/slaghuis/telegram-mcp/internal/session"
	"github.com/slaghuis/telegram-mcp/internal/tools"
)

const instructions = `
This MCP server lets you contact the human user via Telegram.

TOOL USAGE GUIDELINES:
1. telegram_notify  — fire-and-forget status updates. Use for:
     "Starting deployment..." / "Tests passed." / "Build complete."
   Does NOT wait for a reply.

2. telegram_ask_question  — ask an open-ended question. BLOCKS until replied.
   Use for clarifications: "Which database should I use?"
   The user replies to the Telegram message with free text.

3. telegram_request_approval  — present a decision with buttons. BLOCKS.
   Use before ANY destructive action:
   - merging to main
   - deploying to production
   - running destructive migrations
   - deleting data or infrastructure
   - overspending (long cloud-model runs)
   Always include a context argument summarizing WHAT the agent will do.

4. telegram_list_pending  — introspect your own open requests. Call before
   issuing new ones in a long-running loop to avoid flooding the user.

IMPORTANT:
- Prefer FEWER, HIGHER-QUALITY prompts. The user is on a phone.
- Always include a task_tag so multiple parallel tasks are distinguishable.
- Pass a specific, rich 'context' field for approvals.
`

func main() {
	cfgPath := flag.String("config", "config.yaml", "config path")
	mode := flag.String("transport", "sse", "stdio | sse")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := session.NewStore(cfg.Storage.SQLitePath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	hub := session.NewHub(store)

	ctxBoot, cancelBoot := context.WithTimeout(context.Background(), 10*time.Second)
	loaded, expired, err := hub.Rehydrate(ctxBoot)
	cancelBoot()
	if err != nil {
    		logger.Warn("rehydrate failed (continuing with empty hub)", "err", err)
	}
	logger.Info("hub ready", "rehydrated", loaded, "expired_on_startup", expired)


	b := bot.New(cfg.Telegram.Token, cfg.Telegram.ChatID,
		cfg.Telegram.AllowedUserIDs, hub, logger)

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	hub.StartJanitor(ctx, 30*time.Second)

	go func() {
		if err := b.RunPolling(ctx); err != nil && ctx.Err() == nil {
			logger.Error("polling", "err", err)
		}
	}()

	s := server.NewMCPServer(
		"telegram",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithInstructions(instructions),
	)

	defaultTO := time.Duration(cfg.Telegram.DefaultTimeoutS) * time.Second
	tools.RegisterNotify(s, b)
	tools.RegisterAsk(s, b, hub, defaultTO)
	tools.RegisterApproval(s, b, hub, defaultTO)
	tools.RegisterPending(s, hub)

	switch *mode {
	case "stdio":
		logger.Info("starting on stdio")
		if err := server.ServeStdio(s); err != nil {
			log.Fatal(err)
		}
	case "sse":
    		sseServer := server.NewSSEServer(s)
    		mux := http.NewServeMux()
    		mux.Handle("/sse", sseServer)
    		mux.Handle("/message", sseServer)
    		bot.MountPipelineHTTP(mux, b, hub, defaultTO)
		mux.Handle("/metrics", metrics.Handler())

		hub.StartPendingGaugeUpdater(ctx, 5*time.Second) 

    		logger.Info("starting SSE + pipeline HTTP server", "listen", cfg.Listen)
    		if err := http.ListenAndServe(cfg.Listen, mux); err != nil {
        		log.Fatal(err)
    		}
	default:
		log.Fatalf("unknown transport %q", *mode)
	}
}