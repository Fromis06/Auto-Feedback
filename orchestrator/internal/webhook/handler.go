package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/autopatch-engine/orchestrator/internal/config"
	"github.com/autopatch-engine/orchestrator/internal/db"
	"github.com/autopatch-engine/orchestrator/internal/llm"
)

// FeedbackRequest is the JSON body accepted by POST /api/v1/feedback.
type FeedbackRequest struct {
	UserMessage   string         `json:"user_message"`
	AppCommitSHA  string         `json:"app_commit_sha"`
	Breadcrumbs   []any          `json:"breadcrumbs"`
	ConsoleErrors []any          `json:"console_errors"`
	NetworkLogs   []any          `json:"network_logs"`
	DeviceMeta    map[string]any `json:"device_meta"`
}

// Handler serves HTTP endpoints for health checks, feedback ingestion, and GitHub webhooks.
type Handler struct {
	cfg         *config.Config
	store       db.Store
	llmSvc      llm.Service
	gh          *GitHubClient
	rebaser     RebaseRunner
	logger      *slog.Logger
	asyncRebase bool
	rebaseWG    sync.WaitGroup
}

// NewHandler creates a Handler wired with config, DB store, LLM service, GitHub client, and rebase runner.
func NewHandler(cfg *config.Config, store db.Store, llmSvc llm.Service, gh *GitHubClient, rebaser RebaseRunner, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		cfg:         cfg,
		store:       store,
		llmSvc:      llmSvc,
		gh:          gh,
		rebaser:     rebaser,
		logger:      logger,
		asyncRebase: true,
	}
}

// RegisterRoutes mounts all HTTP endpoints using Go 1.22+ method+path routing.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.HandleHealthz)
	mux.HandleFunc("OPTIONS /api/v1/feedback", h.HandleFeedbackOptions)
	mux.HandleFunc("POST /api/v1/feedback", h.HandleFeedback)
	mux.HandleFunc("POST /api/v1/webhooks/github", h.HandleGitHubWebhook)

	// Protected Admin Dashboard & REST API routes (guarded when ADMIN_PASSWORD is set).
	mux.HandleFunc("GET /dashboard", h.requireAdminAuth(h.HandleDashboard))
	mux.HandleFunc("GET /api/v1/tickets", h.requireAdminAuth(h.HandleListTickets))
	mux.HandleFunc("GET /api/v1/stats", h.requireAdminAuth(h.HandleStats))
	mux.HandleFunc("POST /api/v1/tickets/{id}/retry", h.requireAdminAuth(h.HandleRetryTicket))
	mux.HandleFunc("PATCH /api/v1/tickets/{id}", h.requireAdminAuth(h.HandlePatchTicket))
}

func (h *Handler) corsAllowedOrigin() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.CORSAllowedOrigin) != "" {
		return strings.TrimSpace(h.cfg.CORSAllowedOrigin)
	}
	return "*"
}

func (h *Handler) setFeedbackCORSHeaders(w http.ResponseWriter) {
	origin := h.corsAllowedOrigin()
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if origin != "*" {
		w.Header().Add("Vary", "Origin")
	}
}

// HandleFeedbackOptions responds to browser CORS preflight requests for /api/v1/feedback.
func (h *Handler) HandleFeedbackOptions(w http.ResponseWriter, r *http.Request) {
	h.setFeedbackCORSHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

// WaitPendingRebases waits for any background auto-rebase tasks to complete.
func (h *Handler) WaitPendingRebases() {
	h.rebaseWG.Wait()
}

// HandleHealthz verifies database connectivity for Docker healthchecks.
func (h *Handler) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := h.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "degraded",
			"db":     "error",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"db":     "ok",
	})
}

// HandleFeedback ingests feedback, scrubs secrets, runs XML-isolated triage, checks commit
// staleness, deduplicates via pgvector, and queues new tickets.
func (h *Handler) HandleFeedback(w http.ResponseWriter, r *http.Request) {
	h.setFeedbackCORSHeaders(w)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req FeedbackRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}
	if strings.TrimSpace(req.UserMessage) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_message is required"})
		return
	}

	scrubbedMsg, scrubbedConsole, scrubbedNetwork, scrubbedBreadcrumbs := llm.ScrubFeedback(
		req.UserMessage,
		req.ConsoleErrors,
		req.NetworkLogs,
		req.Breadcrumbs,
	)
	scrubbedDevice := map[string]any{}
	if req.DeviceMeta != nil {
		if m, ok := llm.ScrubAny(req.DeviceMeta).(map[string]any); ok {
			scrubbedDevice = m
		}
	}

	telemetry := marshalTelemetry(scrubbedBreadcrumbs, scrubbedConsole, scrubbedNetwork, scrubbedDevice)
	ctx := r.Context()

	triage, err := h.llmSvc.Triage(ctx, llm.TriageInput{
		UserMessage:   scrubbedMsg,
		ConsoleErrors: scrubbedConsole,
		NetworkLogs:   scrubbedNetwork,
	})
	if err != nil {
		h.logger.Error("triage failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "triage service unavailable"})
		return
	}

	switch triage.Classification {
	case llm.ClassSpamOrInjection:
		ticketID, dbErr := h.store.CreateTicketWithTelemetry(ctx, db.CreateTicketParams{
			UserMessage:      scrubbedMsg,
			TechnicalSummary: triage.TechnicalSummary,
			AppCommitSHA:     strings.TrimSpace(req.AppCommitSHA),
			ModuleTag:        triage.ModuleTag,
			Priority:         "LOW",
			Status:           "ABORTED",
			Telemetry:        telemetry,
		})
		if dbErr != nil {
			h.logger.Error("persist rejected spam/injection ticket", "error", dbErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
			return
		}
		if _, runErr := h.store.RecordAgentRun(ctx, db.AgentRunParams{
			TicketID:        ticketID,
			AttemptNumber:   1,
			GuardrailStatus: "PROMPT_INJECTION_DETECTED",
			TestOutput:      "Rejected by triage prompt-injection/spam guard",
		}); runErr != nil {
			h.logger.Error("record prompt injection agent_run", "ticket_id", ticketID, "error", runErr)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ticket_id":      ticketID,
			"status":         "ABORTED",
			"classification": llm.ClassSpamOrInjection,
		})
		return

	case llm.ClassFeatureRequest:
		ticketID, dbErr := h.store.CreateTicketWithTelemetry(ctx, db.CreateTicketParams{
			UserMessage:      scrubbedMsg,
			TechnicalSummary: triage.TechnicalSummary,
			AppCommitSHA:     strings.TrimSpace(req.AppCommitSHA),
			ModuleTag:        triage.ModuleTag,
			Priority:         "LOW",
			Status:           "FEATURE_REQUEST",
			Telemetry:        telemetry,
		})
		if dbErr != nil {
			h.logger.Error("persist feature request ticket", "error", dbErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ticket_id":      ticketID,
			"status":         "FEATURE_REQUEST",
			"classification": llm.ClassFeatureRequest,
		})
		return
	}

	// Skip staleness check gracefully if app_commit_sha is omitted or GitHub is unreachable.
	commitSHA := strings.TrimSpace(req.AppCommitSHA)
	if commitSHA != "" && h.gh != nil {
		if owner, repo, ok := ParseGitHubOwnerRepo(h.cfg.TargetRepoURL); ok {
			outdated, latestSHA, chkErr := h.gh.CheckOutdatedModule(ctx, owner, repo, h.cfg.BaseBranch, commitSHA, triage.ModuleTag)
			if chkErr != nil {
				h.logger.Warn("github outdated check skipped due to error", "error", chkErr)
			} else if outdated {
				ticketID, dbErr := h.store.CreateTicketWithTelemetry(ctx, db.CreateTicketParams{
					UserMessage:      scrubbedMsg,
					TechnicalSummary: triage.TechnicalSummary,
					AppCommitSHA:     commitSHA,
					ModuleTag:        triage.ModuleTag,
					Priority:         triage.Priority,
					Status:           "IGNORED_OUTDATED",
					Telemetry:        telemetry,
				})
				if dbErr != nil {
					h.logger.Error("persist outdated ticket", "error", dbErr)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
					return
				}
				writeJSON(w, http.StatusAccepted, map[string]any{
					"ticket_id":  ticketID,
					"status":     "IGNORED_OUTDATED",
					"latest_sha": latestSHA,
				})
				return
			}
		}
	}

	embedInput := buildEmbeddingInput(triage.TechnicalSummary, scrubbedConsole, scrubbedNetwork)
	vec, err := h.llmSvc.Embed(ctx, embedInput)
	if err != nil {
		h.logger.Error("embedding generation failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "embedding service unavailable"})
		return
	}

	match, err := h.store.FindSimilarOpenTicket(ctx, vec, h.cfg.SimilarityThreshold)
	if err != nil {
		h.logger.Error("similarity search failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	if match != nil {
		newCount, newPriority, incErr := h.store.IncrementTicketOccurrence(ctx, match.ID)
		if incErr != nil {
			h.logger.Error("increment ticket occurrence failed", "ticket_id", match.ID, "error", incErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ticket_id":        match.ID,
			"status":           match.Status,
			"deduplicated":     true,
			"occurrence_count": newCount,
			"priority":         newPriority,
			"similarity":       match.Similarity,
		})
		return
	}

	ticketID, err := h.store.CreateTicketWithTelemetry(ctx, db.CreateTicketParams{
		UserMessage:      scrubbedMsg,
		TechnicalSummary: triage.TechnicalSummary,
		Embedding:        vec,
		AppCommitSHA:     commitSHA,
		ModuleTag:        triage.ModuleTag,
		Priority:         triage.Priority,
		Status:           "QUEUED",
		Telemetry:        telemetry,
	})
	if err != nil {
		h.logger.Error("create queued ticket failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ticket_id":    ticketID,
		"status":       "QUEUED",
		"deduplicated": false,
		"module_tag":   triage.ModuleTag,
		"priority":     triage.Priority,
	})
}

// HandleGitHubWebhook verifies X-Hub-Signature-256 and handles push (auto-rebase ai-fix/* PRs)
// and pull_request closed (merged -> MERGED, closed unmerged -> save human_review_note & ABORTED).
func (h *Handler) HandleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if !VerifyGitHubSignature(h.cfg.GitHubWebhookSecret, payload, sig) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid webhook signature"})
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return

	case "push":
		var pushPayload struct {
			Ref string `json:"ref"`
		}
		if err := json.Unmarshal(payload, &pushPayload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid push payload"})
			return
		}
		if pushPayload.Ref != "refs/heads/"+h.cfg.BaseBranch {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "non-base branch push"})
			return
		}

		runRebase := func(ctx context.Context) {
			if err := h.rebaseOpenPRs(ctx); err != nil {
				h.logger.Error("auto-rebase failed", "error", err)
			}
		}
		if h.asyncRebase {
			h.rebaseWG.Add(1)
			go func() {
				defer h.rebaseWG.Done()
				ctx, cancel := context.WithTimeout(context.Background(), h.cfg.SandboxTimeout)
				defer cancel()
				runRebase(ctx)
			}()
		} else {
			runRebase(r.Context())
		}

		writeJSON(w, http.StatusAccepted, map[string]string{"status": "rebase_scheduled"})
		return

	case "pull_request":
		var prPayload struct {
			Action      string `json:"action"`
			PullRequest struct {
				Number int    `json:"number"`
				Merged bool   `json:"merged"`
				Body   string `json:"body"`
				Head   struct {
					Ref string `json:"ref"`
				} `json:"head"`
			} `json:"pull_request"`
		}
		if err := json.Unmarshal(payload, &prPayload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pull_request payload"})
			return
		}
		if prPayload.Action != "closed" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}

		prNum := prPayload.PullRequest.Number
		fallbackTicketID := ExtractTicketIDFromBranch(prPayload.PullRequest.Head.Ref)

		if prPayload.PullRequest.Merged {
			if err := h.store.MarkTicketMergedByPR(r.Context(), prNum, fallbackTicketID); err != nil {
				h.logger.Warn("mark PR merged failed", "pr_number", prNum, "error", err)
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "MERGED", "pr_number": prNum})
			return
		}

		// Closed without merge: fetch review comments from GitHub and store human_review_note.
		var reviewNote string
		if h.gh != nil {
			if owner, repo, ok := ParseGitHubOwnerRepo(h.cfg.TargetRepoURL); ok {
				fetched, err := h.gh.GetPRReviewComments(r.Context(), owner, repo, prNum)
				if err != nil {
					h.logger.Warn("fetch PR review comments failed", "pr_number", prNum, "error", err)
				} else {
					reviewNote = fetched
				}
			}
		}
		if strings.TrimSpace(reviewNote) == "" {
			reviewNote = fmt.Sprintf("PR #%d closed without merge by reviewer.", prNum)
		}

		if err := h.store.RecordPRReviewRejection(r.Context(), prNum, fallbackTicketID, reviewNote); err != nil {
			h.logger.Warn("record PR review rejection failed", "pr_number", prNum, "error", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":    "ABORTED",
			"pr_number": prNum,
			"recorded":  true,
		})
		return

	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": event})
	}
}

func (h *Handler) rebaseOpenPRs(ctx context.Context) error {
	if h.gh == nil || h.rebaser == nil {
		return nil
	}
	owner, repo, ok := ParseGitHubOwnerRepo(h.cfg.TargetRepoURL)
	if !ok {
		return fmt.Errorf("cannot parse GitHub owner/repo from TARGET_REPO_URL %q", h.cfg.TargetRepoURL)
	}
	prs, err := h.gh.ListOpenAIFixPRs(ctx, owner, repo, h.cfg.BaseBranch)
	if err != nil {
		return fmt.Errorf("list open ai-fix PRs: %w", err)
	}

	var firstErr error
	for _, pr := range prs {
		ticketContext := fmt.Sprintf("PR #%d (%s): %s", pr.Number, pr.Head.Ref, pr.Title)
		if tid := ExtractTicketIDFromBranch(pr.Head.Ref); tid > 0 {
			if ct, err := h.store.GetTicketContext(ctx, tid); err == nil && ct != nil {
				ticketContext = fmt.Sprintf(
					"Ticket #%d (module=%s)\nSummary: %s\nUser Report: %s\nConsole Errors: %s\nNetwork Logs: %s",
					ct.Ticket.ID,
					ct.Ticket.ModuleTag,
					ct.Ticket.TechnicalSummary,
					ct.Ticket.UserMessage,
					string(ct.Telemetry.ConsoleErrors),
					string(ct.Telemetry.NetworkLogs),
				)
			}
		}
		if err := h.rebaser.RebaseAndVerifyPR(ctx, pr, ticketContext); err != nil {
			h.logger.Error("rebase failed for PR", "pr_number", pr.Number, "branch", pr.Head.Ref, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func buildEmbeddingInput(summary string, consoleErrors, networkLogs []any) string {
	consoleBytes, _ := json.Marshal(consoleErrors)
	networkBytes, _ := json.Marshal(networkLogs)
	return fmt.Sprintf(
		"Technical Summary: %s\nConsole Error Signatures: %s\nNetwork Error Signatures: %s",
		summary,
		string(consoleBytes),
		string(networkBytes),
	)
}

func marshalTelemetry(breadcrumbs, consoleErrors, networkLogs []any, deviceMeta map[string]any) db.Telemetry {
	b, _ := json.Marshal(breadcrumbs)
	c, _ := json.Marshal(consoleErrors)
	n, _ := json.Marshal(networkLogs)
	d, _ := json.Marshal(deviceMeta)
	return db.Telemetry{
		Breadcrumbs:   b,
		ConsoleErrors: c,
		NetworkLogs:   n,
		DeviceMeta:    d,
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
