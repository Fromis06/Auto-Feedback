package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/autopatch-engine/orchestrator/internal/config"
	"github.com/autopatch-engine/orchestrator/internal/db"
	"github.com/autopatch-engine/orchestrator/internal/llm"
	"github.com/autopatch-engine/orchestrator/internal/notify"
	"github.com/autopatch-engine/orchestrator/internal/sandbox"
	"github.com/autopatch-engine/orchestrator/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	resolveStartupModels(rootCtx, cfg, logger)

	repo, err := connectDBWithRetry(rootCtx, cfg.DatabaseURL, logger)
	if err != nil {
		logger.Error("failed to connect to PostgreSQL", "error", err)
		os.Exit(1)
	}
	defer repo.Close()

	schemaCtx, cancelSchema := context.WithTimeout(rootCtx, 10*time.Second)
	if err := repo.EnsureSchema(schemaCtx); err != nil {
		cancelSchema()
		logger.Error("failed to apply startup schema migrations", "error", err)
		os.Exit(1)
	}
	cancelSchema()

	dockerRunner, err := sandbox.NewDockerRunner(cfg)
	if err != nil {
		logger.Error("failed to initialize Docker sandbox runner", "error", err)
		os.Exit(1)
	}
	defer func() { _ = dockerRunner.Close() }()

	llmClient := llm.NewClient(
		cfg.LLMBaseURL,
		cfg.LLMAPIKey,
		cfg.TriageModel,
		cfg.CodingModel,
		cfg.EmbeddingModel,
		cfg.VectorDim,
	)
	ghClient := webhook.NewGitHubClient(cfg.GitHubAPIURL, cfg.GitHubToken)
	rebaseRunner := webhook.NewGitRebaseRunner(cfg, llmClient)
	notifier := notify.NewWebhookNotifier(cfg.AlertWebhookURL, cfg.PublicDashboardURL, logger)

	h := webhook.NewHandler(cfg, repo, llmClient, ghClient, rebaseRunner, logger)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	workerCtx, cancelWorkers := context.WithCancel(rootCtx)
	var workerWG sync.WaitGroup
	startWorkerPool(workerCtx, &workerWG, cfg, repo, dockerRunner, notifier, logger)

	serverErrCh := make(chan error, 1)
	go func() {
		logger.Info("orchestrator HTTP server listening", "port", cfg.Port, "dashboard", "/dashboard")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
		close(serverErrCh)
	}()

	select {
	case <-rootCtx.Done():
		logger.Info("shutdown signal received")
	case srvErr := <-serverErrCh:
		if srvErr != nil {
			logger.Error("HTTP server error", "error", srvErr)
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}

	cancelWorkers()
	workerWG.Wait()
	h.WaitPendingRebases()
	notifier.Wait()
	logger.Info("orchestrator shutdown complete")
}

func resolveStartupModels(ctx context.Context, cfg *config.Config, logger *slog.Logger) {
	prefs := llm.ModelPreferences{
		Embedding: cfg.EmbeddingModel,
		Triage:    cfg.TriageModel,
		Coding:    cfg.CodingModel,
	}
	if prefs.NeedsDiscovery() {
		discCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		resolved, err := llm.DiscoverModels(discCtx, cfg.LLMBaseURL, cfg.LLMAPIKey, prefs)
		cancel()
		if err != nil {
			logger.Warn("model auto-discovery failed, applying fallback defaults",
				"llm_base_url", cfg.LLMBaseURL,
				"error", err,
			)
		}
		resolved = resolved.ApplyFallbacks()
		cfg.EmbeddingModel = resolved.Embedding
		cfg.TriageModel = resolved.Triage
		cfg.CodingModel = resolved.Coding
	}

	logger.Info(
		fmt.Sprintf("Resolved Models -> Embedding: %s, Triage: %s, Coding: %s",
			cfg.EmbeddingModel, cfg.TriageModel, cfg.CodingModel),
		"embedding_model", cfg.EmbeddingModel,
		"triage_model", cfg.TriageModel,
		"coding_model", cfg.CodingModel,
		"llm_base_url", cfg.LLMBaseURL,
	)
}

func connectDBWithRetry(ctx context.Context, dsn string, logger *slog.Logger) (*db.Repository, error) {
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		repo, err := db.NewRepository(pingCtx, dsn)
		cancel()
		if err == nil {
			return repo, nil
		}
		lastErr = err
		logger.Warn("waiting for PostgreSQL", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}

func startWorkerPool(
	ctx context.Context,
	wg *sync.WaitGroup,
	cfg *config.Config,
	store db.Store,
	runner sandbox.Runner,
	notifier notify.Notifier,
	logger *slog.Logger,
) {
	for i := 0; i < cfg.WorkerCount; i++ {
		workerID := i + 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWorkerLoop(ctx, workerID, cfg, store, runner, notifier, logger)
		}()
	}
}

func runWorkerLoop(
	ctx context.Context,
	workerID int,
	cfg *config.Config,
	store db.Store,
	runner sandbox.Runner,
	notifier notify.Notifier,
	logger *slog.Logger,
) {
	ticker := time.NewTicker(cfg.WorkerPollInterval)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			return
		}

		processed := processNextTicket(ctx, workerID, cfg, store, runner, notifier, logger)
		if processed {
			// Immediately check for another claimable ticket on a free module_tag.
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func processNextTicket(
	ctx context.Context,
	workerID int,
	cfg *config.Config,
	store db.Store,
	runner sandbox.Runner,
	notifier notify.Notifier,
	logger *slog.Logger,
) bool {
	claimCtx, cancelClaim := context.WithTimeout(ctx, 5*time.Second)
	ct, err := store.ClaimNextQueuedTicket(claimCtx)
	cancelClaim()
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("worker failed to claim ticket", "worker_id", workerID, "error", err)
		}
		return false
	}
	if ct == nil {
		return false
	}

	maxDiffOverride := 0
	if ct.Ticket.MaxDiffOverride != nil && *ct.Ticket.MaxDiffOverride > 0 {
		maxDiffOverride = *ct.Ticket.MaxDiffOverride
	}

	logger.Info("worker claimed ticket",
		"worker_id", workerID,
		"ticket_id", ct.Ticket.ID,
		"module_tag", ct.Ticket.ModuleTag,
		"attempt", ct.AttemptNumber,
		"max_diff_override", maxDiffOverride,
	)

	summary := strings.TrimSpace(ct.Ticket.TechnicalSummary)
	if summary == "" {
		summary = ct.Ticket.UserMessage
	}

	bugPrompt := sandbox.BuildBugPrompt(ct)
	runCtx, cancelRun := context.WithTimeout(ctx, cfg.SandboxTimeout)
	res, runErr := runner.Run(runCtx, sandbox.RunRequest{
		TicketID:        ct.Ticket.ID,
		BugPrompt:       bugPrompt,
		MaxDiffOverride: maxDiffOverride,
	})
	cancelRun()

	persistCtx, cancelPersist := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPersist()

	if runErr != nil {
		logger.Error("sandbox execution error", "worker_id", workerID, "ticket_id", ct.Ticket.ID, "error", runErr)
		_, _ = store.RecordAgentRun(persistCtx, db.AgentRunParams{
			TicketID:        ct.Ticket.ID,
			AttemptNumber:   ct.AttemptNumber,
			TestOutput:      runErr.Error(),
			GuardrailStatus: "RUNTIME_ERROR",
		})
		_ = store.UpdateTicketStatus(persistCtx, ct.Ticket.ID, "ABORTED")
		if notifier != nil {
			notifier.NotifyAsync(notify.Event{
				TicketID:        ct.Ticket.ID,
				ModuleTag:       ct.Ticket.ModuleTag,
				Summary:         summary,
				OccurrenceCount: ct.Ticket.OccurrenceCount,
				GuardrailStatus: "RUNTIME_ERROR",
				TicketStatus:    "ABORTED",
				DashboardURL:    cfg.PublicDashboardURL,
			})
		}
		return true
	}

	_, recErr := store.RecordAgentRun(persistCtx, db.AgentRunParams{
		TicketID:        ct.Ticket.ID,
		AttemptNumber:   ct.AttemptNumber,
		TestOutput:      res.CombinedOutput,
		GuardrailStatus: res.GuardrailStatus,
		PRNumber:        res.PRNumber,
		PRURL:           res.PRURL,
	})
	if recErr != nil {
		logger.Error("record agent_run failed", "ticket_id", ct.Ticket.ID, "error", recErr)
	}

	nextStatus := "ABORTED"
	if res.GuardrailStatus == "PASSED" {
		nextStatus = "PR_OPENED"
	}
	if updErr := store.UpdateTicketStatus(persistCtx, ct.Ticket.ID, nextStatus); updErr != nil {
		logger.Error("update ticket status failed", "ticket_id", ct.Ticket.ID, "status", nextStatus, "error", updErr)
	}

	if notifier != nil {
		notifier.NotifyAsync(notify.Event{
			TicketID:        ct.Ticket.ID,
			ModuleTag:       ct.Ticket.ModuleTag,
			Summary:         summary,
			OccurrenceCount: ct.Ticket.OccurrenceCount,
			GuardrailStatus: res.GuardrailStatus,
			TicketStatus:    nextStatus,
			PRURL:           res.PRURL,
			DashboardURL:    cfg.PublicDashboardURL,
		})
	}

	logger.Info("worker finished ticket",
		"worker_id", workerID,
		"ticket_id", ct.Ticket.ID,
		"guardrail_status", res.GuardrailStatus,
		"next_status", nextStatus,
		"pr_url", res.PRURL,
	)
	return true
}
