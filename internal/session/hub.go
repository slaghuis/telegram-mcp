package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/slaghuis/telegram-mcp/internal/metrics" 
)

type Kind string

const (
	KindNotify   Kind = "notify"
	KindAsk      Kind = "ask"
	KindApproval Kind = "approval"
)

type Status string

const (
	StatusPending  Status = "pending"
	StatusAnswered Status = "answered"
	StatusTimedOut Status = "timeout"
	StatusCanceled Status = "canceled"
)

type Reply struct {
	Choice string
	Text   string
	Status Status
}

type Session struct {
	ID         string
	Kind       Kind
	Prompt     string
	Options    []string
	Context    string
	MessageID  int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	TaskTag    string

	// Orphaned sessions were loaded from disk after a restart.
	// No in-process waiter is listening on replyCh.
	Orphaned bool

	replyCh chan Reply
	done    bool
	mu      sync.Mutex
}

func (s *Session) deliver(r Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	if s.replyCh != nil {
		select {
		case s.replyCh <- r:
		default:
		}
		close(s.replyCh)
	}
}

type Hub struct {
	mu       sync.Mutex
	sessions map[string]*Session
	store    *Store
	log      *slog.Logger
}

func NewHub(store *Store, log *slog.Logger) *Hub {
	return &Hub{
		sessions: make(map[string]*Session),
		store:    store,
		log:      log,
	}
}

// Register adds a new session and persists it. Fails if a session with
// this ID already exists.
func (h *Hub) Register(s *Session) error {
	s.replyCh = make(chan Reply, 1)
	h.mu.Lock()
	if _, exists := h.sessions[s.ID]; exists {
		h.mu.Unlock()
		return errors.New("session id already in use")
	}
	h.sessions[s.ID] = s
	h.mu.Unlock()
	if err := h.store.Save(s); err != nil {
		return err
	}
	metrics.SessionsCreated.WithLabelValues(string(s.Kind)).Inc() 
	return nil
}

// RegisterIdempotent is like Register but allows reusing an existing ID.
// If the ID already exists in SQLite as pending, returns the existing session.
// If it exists but was already resolved, returns (nil, false, nil) — caller
// should fetch the resolved state via Store.Get instead.
// If it doesn't exist, creates a fresh pending session.
func (h *Hub) RegisterIdempotent(s *Session) (sess *Session, created bool, err error) {
	s.replyCh = make(chan Reply, 1)

	h.mu.Lock()
	if existing, ok := h.sessions[s.ID]; ok {
		h.mu.Unlock()
		return existing, false, nil
	}
	h.mu.Unlock()

	inserted, err := h.store.SaveIfAbsent(s)
	if err != nil {
		return nil, false, err
	}
	if !inserted {
		// Row exists in DB but not in memory — it was resolved before this call.
		return nil, false, nil
	}
	h.mu.Lock()
	h.sessions[s.ID] = s
	h.mu.Unlock()
	metrics.SessionsCreated.WithLabelValues(string(s.Kind)).Inc()   // ← add
	return s, true, nil
}

func (h *Hub) Get(id string) (*Session, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	return s, ok
}

func (h *Hub) GetByMessage(messageID int64) (*Session, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.MessageID == messageID {
			return s, true
		}
	}
	return nil, false
}

func (h *Hub) Resolve(id string, r Reply) error {
	s, ok := h.Get(id)
	if !ok {
		// No in-memory session (e.g. late tap after janitor expired it).
		// Still persist resolution to disk.
		return h.store.Resolve(id, r)
	}
	s.deliver(r)
	h.mu.Lock()
	delete(h.sessions, id)
	h.mu.Unlock()
	err := h.store.Resolve(id, r)

	// ---- metrics ----
	metrics.SessionsResolved.WithLabelValues(string(s.Kind), string(r.Status)).Inc()
	if r.Status == StatusAnswered && s.Kind == KindApproval {
		metrics.ApprovalLatency.
			WithLabelValues(s.TaskTag).
			Observe(time.Since(s.CreatedAt).Seconds())
	}
	// -----------------

	return err
}

func (h *Hub) Wait(ctx context.Context, s *Session) Reply {
	if s.replyCh == nil {
		// Orphaned — nothing to wait on. Shouldn't happen in practice;
		// orphaned sessions aren't exposed to new Wait callers.
		return Reply{Status: StatusCanceled}
	}
	timer := time.NewTimer(time.Until(s.ExpiresAt))
	defer timer.Stop()
	select {
	case r, ok := <-s.replyCh:
		if !ok {
			return Reply{Status: StatusCanceled}
		}
		return r
	case <-timer.C:
		timeoutReply := Reply{Status: StatusTimedOut}
		s.deliver(timeoutReply)
		h.mu.Lock()
		delete(h.sessions, s.ID)
		h.mu.Unlock()
		_ = h.store.Resolve(s.ID, timeoutReply)
		metrics.SessionsResolved.WithLabelValues(string(s.Kind), string(StatusTimedOut)).Inc()   // ← add
		return timeoutReply
	case <-ctx.Done():
		return Reply{Status: StatusCanceled}
	}
}

func (h *Hub) ListPending() []*Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	return out
}

func (h *Hub) UpdateMessageID(id string, msgID int64) {
	s, ok := h.Get(id)
	if !ok {
		return
	}
	s.MessageID = msgID
	_ = h.store.UpdateMessageID(id, msgID)
}

// Rehydrate loads all pending sessions from storage into memory.
// Call this ONCE on startup, before any Telegram polling or HTTP serving begins.
//
// Behaviour:
//   - Rows whose expires_at has already passed are moved to 'timeout' first.
//   - Remaining pending rows are loaded as "orphaned" Sessions with a nil replyCh.
//   - Taps on their buttons will resolve the DB row and edit the message,
//     but no in-process agent will receive the reply.
//   - A new Register() with the same ID will fail (we keep the orphan slot),
//     ensuring button callbacks still resolve correctly.
func (h *Hub) Rehydrate(ctx context.Context) (loaded, expired int, err error) {
	expired, err = h.store.ExpireOverdue()
	if err != nil {
		return 0, 0, err
	}
	rows, err := h.store.LoadPending()
	if err != nil {
		return 0, expired, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, row := range rows {
		if _, exists := h.sessions[row.ID]; exists {
			continue
		}
		s := &Session{
			ID:        row.ID,
			Kind:      row.Kind,
			Prompt:    row.Prompt,
			Options:   row.Options,
			Context:   row.Context,
			MessageID: row.MessageID,
			TaskTag:   row.TaskTag,
			CreatedAt: row.CreatedAt,
			ExpiresAt: row.ExpiresAt,
			Orphaned:  true,
			replyCh:   nil, // explicit — nothing to wait on
		}
		h.sessions[s.ID] = s
		_ = h.store.MarkOrphaned(s.ID)
		loaded++
	}
	if h.log != nil {
		h.log.Info("rehydrate complete",
			"loaded", loaded, "expired_on_startup", expired)
	}
	if loaded > 0 {
		metrics.Rehydrated.Add(float64(loaded))   // ← add
	}
	return loaded, expired, nil
}

// StartJanitor runs a goroutine that periodically expires in-memory
// pending sessions whose deadlines have passed (covers both freshly
// created and rehydrated sessions).
func (h *Hub) StartJanitor(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.sweep()
			}
		}
	}()
}

func (h *Hub) sweep() {
	now := time.Now()
	var expire []*Session
	h.mu.Lock()
	for _, s := range h.sessions {
		if now.After(s.ExpiresAt) {
			expire = append(expire, s)
		}
	}
	h.mu.Unlock()
	for _, s := range expire {
		s.deliver(Reply{Status: StatusTimedOut})
		h.mu.Lock()
		delete(h.sessions, s.ID)
		h.mu.Unlock()
		_ = h.store.Resolve(s.ID, Reply{Status: StatusTimedOut})
		metrics.SessionsResolved.WithLabelValues(string(s.Kind), string(StatusTimedOut)).Inc()   // ← add
	}
}

// StoreGet exposes the underlying store's Get for HTTP handlers.
// Hub owns the Store; callers don't need to.
func (h *Hub) StoreGet(id string) (ResolvedRow, error) {
	return h.store.Get(id)
}

// StartPendingGaugeUpdater samples the pending-sessions count every `every`
// duration and updates the Prometheus gauge. Call once from main after Rehydrate.
func (h *Hub) StartPendingGaugeUpdater(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		// Initial sample so the metric is populated immediately after boot.
		metrics.PendingGauge.Set(float64(len(h.ListPending())))
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				metrics.PendingGauge.Set(float64(len(h.ListPending())))
			}
		}
	}()
}