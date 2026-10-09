package bot

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/slaghuis/telegram-mcp/internal/session"
)

func MountPipelineHTTP(mux *http.ServeMux, b *Bot, hub *session.Hub, defaultTO time.Duration) {
	mux.HandleFunc("/pipeline/notify", notifyHandler(b))
	mux.HandleFunc("/pipeline/approve", approveHandler(b, hub, defaultTO))
	mux.HandleFunc("/pipeline/approve/", approveStatusHandler(hub))
}

func notifyHandler(b *Bot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Message string `json:"message"`
			TaskTag string `json:"task_tag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := b.SendMessage(r.Context(),
			FormatNotify(req.Message, req.TaskTag), nil, ""); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func approveHandler(b *Bot, hub *session.Hub, defaultTO time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SessionID      string   `json:"session_id"` // optional — stable retry ID
			Prompt         string   `json:"prompt"`
			Context        string   `json:"context"`
			Options        []string `json:"options"`
			TaskTag        string   `json:"task_tag"`
			TimeoutSeconds int      `json:"timeout_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Prompt == "" {
			http.Error(w, "prompt required", http.StatusBadRequest)
			return
		}
		if len(req.Options) == 0 {
			req.Options = []string{"Approve", "Reject"}
		}
		timeout := defaultTO
		if req.TimeoutSeconds > 0 {
			timeout = time.Duration(req.TimeoutSeconds) * time.Second
		}
		id := req.SessionID
		if id == "" {
			id = uuid.NewString()
		}

		sess := &session.Session{
			ID:        id,
			Kind:      session.KindApproval,
			Prompt:    req.Prompt,
			Options:   req.Options,
			Context:   req.Context,
			TaskTag:   req.TaskTag,
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(timeout),
		}

		// Idempotent: if session_id provided and already resolved, return state.
		live, created, err := hub.RegisterIdempotent(sess)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if live == nil {
			// Already resolved before this request arrived — look up outcome.
			resolved, lerr := hub.StoreGet(id) // helper below
			if lerr != nil {
				http.Error(w, lerr.Error(), http.StatusInternalServerError)
				return
			}
			writeResolved(w, resolved)
			return
		}

		if created {
			msgID, err := b.SendMessage(r.Context(),
				FormatApproval(req.Prompt, req.Context, req.TaskTag),
				req.Options, live.ID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			hub.UpdateMessageID(live.ID, msgID)
		}
		// Else: existing in-memory session (concurrent retry) — join its waiter.

		reply := hub.Wait(r.Context(), live)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": live.ID,
			"status":     string(reply.Status),
			"choice":     reply.Choice,
		})
	}
}

// GET /pipeline/approve/{id} — polling endpoint for retrying callers.
func approveStatusHandler(hub *session.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(path.Clean(r.URL.Path), "/pipeline/approve/")
		if id == "" || id == "." {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		resolved, err := hub.StoreGet(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeResolved(w, resolved)
	}
}

func writeResolved(w http.ResponseWriter, r session.ResolvedRow) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id":  r.ID,
		"status":      string(r.Status),
		"choice":      r.Choice,
		"text":        r.Text,
		"orphaned":    r.Orphaned,
		"resolved_at": r.ResolvedAt,
	})
}