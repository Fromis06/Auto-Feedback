package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/autopatch-engine/orchestrator/internal/config"
	"github.com/autopatch-engine/orchestrator/internal/db"
	"github.com/autopatch-engine/orchestrator/internal/notify"
	"github.com/autopatch-engine/orchestrator/internal/sandbox"
)

type stubStore struct {
	claimed      *db.ClaimedTicket
	runs         []db.AgentRunParams
	statusUpdate string
}

func (s *stubStore) Ping(context.Context) error         { return nil }
func (s *stubStore) EnsureSchema(context.Context) error { return nil }
func (s *stubStore) FindSimilarOpenTicket(context.Context, []float32, float64) (*db.SimilarTicketMatch, error) {
	return nil, nil
}
func (s *stubStore) IncrementTicketOccurrence(context.Context, int64) (int, string, error) {
	return 1, "MEDIUM", nil
}
func (s *stubStore) CreateTicketWithTelemetry(context.Context, db.CreateTicketParams) (int64, error) {
	return 1, nil
}
func (s *stubStore) RecordAgentRun(_ context.Context, p db.AgentRunParams) (int64, error) {
	s.runs = append(s.runs, p)
	return 1, nil
}
func (s *stubStore) ClaimNextQueuedTicket(context.Context) (*db.ClaimedTicket, error) {
	ct := s.claimed
	s.claimed = nil
	return ct, nil
}
func (s *stubStore) UpdateTicketStatus(_ context.Context, _ int64, status string) error {
	s.statusUpdate = status
	return nil
}
func (s *stubStore) GetTicketContext(context.Context, int64) (*db.ClaimedTicket, error) {
	return nil, db.ErrNotFound
}
func (s *stubStore) RecordPRReviewRejection(context.Context, int, int64, string) error { return nil }
func (s *stubStore) MarkTicketMergedByPR(context.Context, int, int64) error            { return nil }
func (s *stubStore) GetPastReviewNotes(context.Context, string, int) ([]string, error) {
	return nil, nil
}
func (s *stubStore) ListTickets(context.Context, db.TicketListFilter) ([]db.TicketListItem, int, error) {
	return nil, 0, nil
}
func (s *stubStore) GetDashboardStats(context.Context) (*db.DashboardStats, error) {
	return &db.DashboardStats{}, nil
}
func (s *stubStore) RetryTicket(context.Context, int64, *int) (*db.Ticket, error) {
	return nil, db.ErrNotFound
}
func (s *stubStore) PatchTicket(context.Context, int64, db.TicketUpdateParams) (*db.Ticket, error) {
	return nil, db.ErrNotFound
}

type stubRunner struct {
	res     *sandbox.RunResult
	err     error
	lastReq sandbox.RunRequest
}

func (r *stubRunner) Run(_ context.Context, req sandbox.RunRequest) (*sandbox.RunResult, error) {
	r.lastReq = req
	return r.res, r.err
}

type stubNotifier struct {
	events []notify.Event
}

func (n *stubNotifier) NotifyAsync(e notify.Event)            { n.events = append(n.events, e) }
func (n *stubNotifier) Send(context.Context, notify.Event) error { return nil }
func (n *stubNotifier) Wait()                                 {}

func TestProcessNextTicketSuccessFailureOverrideAndNotify(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		SandboxTimeout:     5 * time.Second,
		PublicDashboardURL: "https://autopatch.example.com/dashboard",
	}

	prNum := 19
	diffOverride := 500
	store := &stubStore{
		claimed: &db.ClaimedTicket{
			Ticket: db.Ticket{
				ID:               10,
				ModuleTag:        "checkout",
				Priority:         "HIGH",
				OccurrenceCount:  3,
				TechnicalSummary: "Fix total calculation",
				MaxDiffOverride:  &diffOverride,
			},
			AttemptNumber: 1,
		},
	}
	runner := &stubRunner{
		res: &sandbox.RunResult{
			ExitCode:        0,
			GuardrailStatus: "PASSED",
			PRNumber:        &prNum,
			PRURL:           "https://github.com/org/repo/pull/19",
			CombinedOutput:  "PR_URL=https://github.com/org/repo/pull/19",
		},
	}
	notifier := &stubNotifier{}

	if !processNextTicket(context.Background(), 1, cfg, store, runner, notifier, logger) {
		t.Fatalf("expected ticket to be processed")
	}
	if store.statusUpdate != "PR_OPENED" {
		t.Fatalf("expected status PR_OPENED, got %q", store.statusUpdate)
	}
	if runner.lastReq.MaxDiffOverride != 500 {
		t.Fatalf("expected MaxDiffOverride 500 passed to sandbox runner, got %d", runner.lastReq.MaxDiffOverride)
	}
	if len(store.runs) != 1 || store.runs[0].GuardrailStatus != "PASSED" || *store.runs[0].PRNumber != 19 {
		t.Fatalf("unexpected agent_run recorded: %+v", store.runs)
	}
	if len(notifier.events) != 1 || notifier.events[0].GuardrailStatus != "PASSED" || notifier.events[0].PRURL != "https://github.com/org/repo/pull/19" {
		t.Fatalf("expected PASSED notification event, got %+v", notifier.events)
	}

	// Guardrail failure -> ABORTED + alert
	storeFail := &stubStore{
		claimed: &db.ClaimedTicket{
			Ticket:        db.Ticket{ID: 11, ModuleTag: "auth"},
			AttemptNumber: 2,
		},
	}
	runnerFail := &stubRunner{
		res: &sandbox.RunResult{
			ExitCode:        11,
			GuardrailStatus: "BLOCKED_PROTECTED_PATH",
			CombinedOutput:  "touched .env",
		},
	}
	notifierFail := &stubNotifier{}
	processNextTicket(context.Background(), 1, cfg, storeFail, runnerFail, notifierFail, logger)
	if storeFail.statusUpdate != "ABORTED" || storeFail.runs[0].GuardrailStatus != "BLOCKED_PROTECTED_PATH" {
		t.Fatalf("expected ABORTED / BLOCKED_PROTECTED_PATH, got %q / %+v", storeFail.statusUpdate, storeFail.runs)
	}
	if len(notifierFail.events) != 1 || notifierFail.events[0].GuardrailStatus != "BLOCKED_PROTECTED_PATH" {
		t.Fatalf("expected ABORTED guardrail notification, got %+v", notifierFail.events)
	}

	// Runner error -> RUNTIME_ERROR + ABORTED + alert
	storeErr := &stubStore{
		claimed: &db.ClaimedTicket{
			Ticket:        db.Ticket{ID: 12, ModuleTag: "billing"},
			AttemptNumber: 1,
		},
	}
	runnerErr := &stubRunner{err: errors.New("docker daemon unavailable")}
	notifierErr := &stubNotifier{}
	processNextTicket(context.Background(), 1, cfg, storeErr, runnerErr, notifierErr, logger)
	if storeErr.statusUpdate != "ABORTED" || storeErr.runs[0].GuardrailStatus != "RUNTIME_ERROR" {
		t.Fatalf("expected ABORTED / RUNTIME_ERROR, got %q / %+v", storeErr.statusUpdate, storeErr.runs)
	}
	if len(notifierErr.events) != 1 || notifierErr.events[0].GuardrailStatus != "RUNTIME_ERROR" {
		t.Fatalf("expected RUNTIME_ERROR notification, got %+v", notifierErr.events)
	}
}

func TestResolveStartupModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"id": "nomic-embed-text"},
				{"id": "qwen2.5-coder:7b"},
				{"id": "llama3.2:3b"},
			},
		})
	}))
	defer srv.Close()

	cfg := &config.Config{
		LLMBaseURL:     srv.URL,
		LLMAPIKey:      "ollama",
		EmbeddingModel: "auto",
		TriageModel:    "auto",
		CodingModel:    "auto",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	resolveStartupModels(context.Background(), cfg, logger)

	if cfg.EmbeddingModel != "nomic-embed-text" || cfg.TriageModel != "llama3.2:3b" || cfg.CodingModel != "qwen2.5-coder:7b" {
		t.Fatalf("unexpected resolved startup models: emb=%q triage=%q coding=%q", cfg.EmbeddingModel, cfg.TriageModel, cfg.CodingModel)
	}
}
