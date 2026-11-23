package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const claimAdvisoryLockID int64 = 789012

// Repository implements Store using a pgxpool.Pool backed by PostgreSQL + pgvector.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository opens a pgxpool connection pool and verifies connectivity.
func NewRepository(ctx context.Context, databaseURL string) (*Repository, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Repository{pool: pool}, nil
}

// NewRepositoryFromPool wraps an existing pgxpool.Pool.
func NewRepositoryFromPool(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Close closes the underlying connection pool.
func (r *Repository) Close() {
	if r.pool != nil {
		r.pool.Close()
	}
}

// Ping checks PostgreSQL health.
func (r *Repository) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}

// EnsureSchema idempotently applies additive migrations so upgraded orchestrator
// builds work cleanly against existing PostgreSQL volumes.
func (r *Repository) EnsureSchema(ctx context.Context) error {
	statements := []string{
		`ALTER TABLE feedback_tickets ADD COLUMN IF NOT EXISTS max_diff_override INTEGER`,
		`ALTER TABLE feedback_tickets DROP CONSTRAINT IF EXISTS feedback_tickets_status_check`,
		`ALTER TABLE feedback_tickets ADD CONSTRAINT feedback_tickets_status_check CHECK (status IN ('QUEUED', 'RUNNING', 'PR_OPENED', 'MERGED', 'ABORTED', 'IGNORED_OUTDATED', 'FEATURE_REQUEST'))`,
		`ALTER TABLE feedback_tickets DROP CONSTRAINT IF EXISTS feedback_tickets_priority_check`,
		`ALTER TABLE feedback_tickets ADD CONSTRAINT feedback_tickets_priority_check CHECK (priority IN ('LOW', 'MEDIUM', 'NORMAL', 'HIGH', 'CRITICAL'))`,
	}
	for _, stmt := range statements {
		if _, err := r.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure schema (%s): %w", stmt, err)
		}
	}
	return nil
}

// FormatVector formats a float32 slice as a pgvector literal string "[v1,v2,...]".
func FormatVector(vec []float32) string {
	var b strings.Builder
	b.Grow(len(vec)*10 + 2)
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// FindSimilarOpenTicket finds the nearest open ticket (QUEUED, RUNNING, PR_OPENED)
// whose cosine similarity (1 - cosine_distance) strictly exceeds threshold.
func (r *Repository) FindSimilarOpenTicket(ctx context.Context, embedding []float32, threshold float64) (*SimilarTicketMatch, error) {
	if len(embedding) == 0 {
		return nil, nil
	}
	vecStr := FormatVector(embedding)

	const query = `
SELECT id, module_tag, occurrence_count, priority, status,
       1 - (embedding <=> $1::vector) AS similarity
FROM feedback_tickets
WHERE embedding IS NOT NULL
  AND status IN ('QUEUED', 'RUNNING', 'PR_OPENED')
ORDER BY embedding <=> $1::vector ASC
LIMIT 1`

	var match SimilarTicketMatch
	err := r.pool.QueryRow(ctx, query, vecStr).Scan(
		&match.ID,
		&match.ModuleTag,
		&match.OccurrenceCount,
		&match.Priority,
		&match.Status,
		&match.Similarity,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query similar open ticket: %w", err)
	}
	if !isSimilarityMatch(match.Similarity, threshold) {
		return nil, nil
	}
	return &match, nil
}

func isSimilarityMatch(similarity, threshold float64) bool {
	return !math.IsNaN(similarity) && similarity > threshold
}

// IncrementTicketOccurrence atomically increments occurrence_count and escalates
// priority (HIGH at >=3, CRITICAL at >=5).
func (r *Repository) IncrementTicketOccurrence(ctx context.Context, ticketID int64) (int, string, error) {
	const query = `
UPDATE feedback_tickets
SET occurrence_count = occurrence_count + 1,
    priority = CASE
        WHEN occurrence_count + 1 >= 5 THEN 'CRITICAL'
        WHEN occurrence_count + 1 >= 3 THEN
            CASE WHEN priority = 'CRITICAL' THEN 'CRITICAL' ELSE 'HIGH' END
        ELSE priority
    END,
    updated_at = NOW()
WHERE id = $1
RETURNING occurrence_count, priority`

	var count int
	var priority string
	err := r.pool.QueryRow(ctx, query, ticketID).Scan(&count, &priority)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("increment occurrence_count: %w", err)
	}
	return count, priority, nil
}

// CreateTicketWithTelemetry inserts a feedback_tickets row and its linked ticket_telemetry row in one transaction.
func (r *Repository) CreateTicketWithTelemetry(ctx context.Context, params CreateTicketParams) (int64, error) {
	priority := params.Priority
	if priority == "" {
		priority = "MEDIUM"
	}
	status := params.Status
	if status == "" {
		status = "QUEUED"
	}
	moduleTag := params.ModuleTag
	if moduleTag == "" {
		moduleTag = "general"
	}

	var vecParam any
	if len(params.Embedding) > 0 {
		vecParam = FormatVector(params.Embedding)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin create ticket tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insertTicketSQL = `
INSERT INTO feedback_tickets (
    user_message, technical_summary, embedding, app_commit_sha,
    module_tag, priority, status
)
VALUES ($1, $2, $3::vector, $4, $5, $6, $7)
RETURNING id`

	var ticketID int64
	err = tx.QueryRow(
		ctx,
		insertTicketSQL,
		params.UserMessage,
		params.TechnicalSummary,
		vecParam,
		params.AppCommitSHA,
		moduleTag,
		priority,
		status,
	).Scan(&ticketID)
	if err != nil {
		return 0, fmt.Errorf("insert feedback_tickets: %w", err)
	}

	const insertTelemetrySQL = `
INSERT INTO ticket_telemetry (
    ticket_id, breadcrumbs, console_errors, network_logs, device_meta
)
VALUES ($1, $2::jsonb, $3::jsonb, $4::jsonb, $5::jsonb)`

	_, err = tx.Exec(
		ctx,
		insertTelemetrySQL,
		ticketID,
		normalizeJSON(params.Telemetry.Breadcrumbs, "[]"),
		normalizeJSON(params.Telemetry.ConsoleErrors, "[]"),
		normalizeJSON(params.Telemetry.NetworkLogs, "[]"),
		normalizeJSON(params.Telemetry.DeviceMeta, "{}"),
	)
	if err != nil {
		return 0, fmt.Errorf("insert ticket_telemetry: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit create ticket tx: %w", err)
	}
	return ticketID, nil
}

// RecordAgentRun inserts an execution record into agent_runs.
func (r *Repository) RecordAgentRun(ctx context.Context, params AgentRunParams) (int64, error) {
	attempt := params.AttemptNumber
	if attempt <= 0 {
		attempt = 1
	}
	modFiles := params.ModifiedFiles
	if modFiles == nil {
		modFiles = []string{}
	}

	const query = `
INSERT INTO agent_runs (
    ticket_id, attempt_number, modified_files, test_output,
    guardrail_status, pr_number, pr_url, human_review_note, tokens_used
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id`

	var id int64
	err := r.pool.QueryRow(
		ctx,
		query,
		params.TicketID,
		attempt,
		modFiles,
		params.TestOutput,
		params.GuardrailStatus,
		params.PRNumber,
		params.PRURL,
		params.HumanReviewNote,
		params.TokensUsed,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert agent_runs: %w", err)
	}
	return id, nil
}

// ClaimNextQueuedTicket claims the highest-priority QUEUED ticket whose module_tag
// has no RUNNING or PR_OPENED ticket, using FOR UPDATE OF t SKIP LOCKED and immediately
// transitioning the claimed ticket to RUNNING inside the same transaction.
func (r *Repository) ClaimNextQueuedTicket(ctx context.Context) (*ClaimedTicket, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize concurrent claim transactions briefly so READ COMMITTED workers
	// always observe newly committed RUNNING rows for the same module_tag.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", claimAdvisoryLockID); err != nil {
		return nil, fmt.Errorf("acquire claim advisory lock: %w", err)
	}

	const selectSQL = `
SELECT t.id, t.user_message, t.technical_summary, t.app_commit_sha,
       t.module_tag, t.occurrence_count, t.priority, t.status,
       t.max_diff_override, t.created_at, t.updated_at
FROM feedback_tickets t
WHERE t.status = 'QUEUED'
  AND NOT EXISTS (
      SELECT 1 FROM feedback_tickets busy
      WHERE busy.module_tag = t.module_tag
        AND busy.status IN ('RUNNING', 'PR_OPENED')
  )
ORDER BY
    CASE t.priority
        WHEN 'CRITICAL' THEN 1
        WHEN 'HIGH' THEN 2
        WHEN 'MEDIUM' THEN 3
        WHEN 'NORMAL' THEN 3
        ELSE 4
    END ASC,
    t.created_at ASC
LIMIT 1
FOR UPDATE OF t SKIP LOCKED`

	var claimed ClaimedTicket
	err = tx.QueryRow(ctx, selectSQL).Scan(
		&claimed.Ticket.ID,
		&claimed.Ticket.UserMessage,
		&claimed.Ticket.TechnicalSummary,
		&claimed.Ticket.AppCommitSHA,
		&claimed.Ticket.ModuleTag,
		&claimed.Ticket.OccurrenceCount,
		&claimed.Ticket.Priority,
		&claimed.Ticket.Status,
		&claimed.Ticket.MaxDiffOverride,
		&claimed.Ticket.CreatedAt,
		&claimed.Ticket.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim queued ticket: %w", err)
	}

	const updateSQL = `
UPDATE feedback_tickets
SET status = 'RUNNING', updated_at = NOW()
WHERE id = $1
RETURNING status, updated_at`
	if err := tx.QueryRow(ctx, updateSQL, claimed.Ticket.ID).Scan(&claimed.Ticket.Status, &claimed.Ticket.UpdatedAt); err != nil {
		return nil, fmt.Errorf("mark claimed ticket running: %w", err)
	}

	if err := loadTicketExtras(ctx, tx, &claimed); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim tx: %w", err)
	}
	return &claimed, nil
}

// UpdateTicketStatus updates the status and updated_at timestamp of a ticket.
func (r *Repository) UpdateTicketStatus(ctx context.Context, ticketID int64, status string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE feedback_tickets SET status = $2, updated_at = NOW() WHERE id = $1`,
		ticketID, status,
	)
	if err != nil {
		return fmt.Errorf("update ticket status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetTicketContext retrieves a ticket along with its telemetry and past module review notes.
func (r *Repository) GetTicketContext(ctx context.Context, ticketID int64) (*ClaimedTicket, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin get ticket context tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectSQL = `
SELECT id, user_message, technical_summary, app_commit_sha,
       module_tag, occurrence_count, priority, status,
       max_diff_override, created_at, updated_at
FROM feedback_tickets
WHERE id = $1`

	var ct ClaimedTicket
	err = tx.QueryRow(ctx, selectSQL, ticketID).Scan(
		&ct.Ticket.ID,
		&ct.Ticket.UserMessage,
		&ct.Ticket.TechnicalSummary,
		&ct.Ticket.AppCommitSHA,
		&ct.Ticket.ModuleTag,
		&ct.Ticket.OccurrenceCount,
		&ct.Ticket.Priority,
		&ct.Ticket.Status,
		&ct.Ticket.MaxDiffOverride,
		&ct.Ticket.CreatedAt,
		&ct.Ticket.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select ticket %d: %w", ticketID, err)
	}

	if err := loadTicketExtras(ctx, tx, &ct); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit get ticket context tx: %w", err)
	}
	return &ct, nil
}

// RecordPRReviewRejection saves human review comments to agent_runs.human_review_note
// and transitions the associated ticket to ABORTED.
func (r *Repository) RecordPRReviewRejection(ctx context.Context, prNumber int, fallbackTicketID int64, reviewNote string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin pr rejection tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var runID, ticketID int64
	err = tx.QueryRow(ctx, `
SELECT id, ticket_id
FROM agent_runs
WHERE pr_number = $1
ORDER BY created_at DESC
LIMIT 1`, prNumber).Scan(&runID, &ticketID)

	if errors.Is(err, pgx.ErrNoRows) && fallbackTicketID > 0 {
		err = tx.QueryRow(ctx, `
SELECT id, ticket_id
FROM agent_runs
WHERE ticket_id = $1
ORDER BY created_at DESC
LIMIT 1`, fallbackTicketID).Scan(&runID, &ticketID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if fallbackTicketID > 0 {
			if _, uErr := tx.Exec(ctx, `UPDATE feedback_tickets SET status = 'ABORTED', updated_at = NOW() WHERE id = $1`, fallbackTicketID); uErr != nil {
				return fmt.Errorf("abort fallback ticket: %w", uErr)
			}
			return tx.Commit(ctx)
		}
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup agent_run for pr #%d: %w", prNumber, err)
	}

	if _, err := tx.Exec(ctx, `UPDATE agent_runs SET human_review_note = $2 WHERE id = $1`, runID, reviewNote); err != nil {
		return fmt.Errorf("update human_review_note: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE feedback_tickets SET status = 'ABORTED', updated_at = NOW() WHERE id = $1`, ticketID); err != nil {
		return fmt.Errorf("abort ticket %d: %w", ticketID, err)
	}
	return tx.Commit(ctx)
}

// MarkTicketMergedByPR updates the ticket associated with prNumber (or fallbackTicketID) to MERGED.
func (r *Repository) MarkTicketMergedByPR(ctx context.Context, prNumber int, fallbackTicketID int64) error {
	var ticketID int64
	err := r.pool.QueryRow(ctx, `
SELECT ticket_id
FROM agent_runs
WHERE pr_number = $1
ORDER BY created_at DESC
LIMIT 1`, prNumber).Scan(&ticketID)

	if errors.Is(err, pgx.ErrNoRows) && fallbackTicketID > 0 {
		ticketID = fallbackTicketID
	} else if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lookup ticket for merged PR #%d: %w", prNumber, err)
	}

	return r.UpdateTicketStatus(ctx, ticketID, "MERGED")
}

// GetPastReviewNotes fetches non-empty human_review_note entries for a module_tag.
func (r *Repository) GetPastReviewNotes(ctx context.Context, moduleTag string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := r.pool.Query(ctx, `
SELECT ar.human_review_note
FROM agent_runs ar
JOIN feedback_tickets ft ON ft.id = ar.ticket_id
WHERE ft.module_tag = $1
  AND ar.human_review_note <> ''
ORDER BY ar.created_at DESC
LIMIT $2`, moduleTag, limit)
	if err != nil {
		return nil, fmt.Errorf("query past review notes: %w", err)
	}
	defer rows.Close()

	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return nil, fmt.Errorf("scan review note: %w", err)
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

// ListTickets returns feedback tickets joined with their latest agent_run and ticket_telemetry.
func (r *Repository) ListTickets(ctx context.Context, filter TicketListFilter) ([]TicketListItem, int, error) {
	status := strings.ToUpper(strings.TrimSpace(filter.Status))
	if status == "ALL" {
		status = ""
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	var total int
	if err := r.pool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM feedback_tickets WHERE ($1 = '' OR status = $1)`,
		status,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count tickets: %w", err)
	}

	const query = `
SELECT
    t.id, t.user_message, t.technical_summary, t.app_commit_sha,
    t.module_tag, t.occurrence_count, t.priority, t.status,
    t.max_diff_override, t.created_at, t.updated_at,
    COALESCE(r.attempt_number, 0),
    COALESCE(r.guardrail_status, ''),
    r.pr_number,
    COALESCE(r.pr_url, ''),
    COALESCE(r.test_output, ''),
    COALESCE(r.human_review_note, ''),
    COALESCE(r.tokens_used, 0),
    COALESCE(tel.breadcrumbs, '[]'::jsonb),
    COALESCE(tel.console_errors, '[]'::jsonb),
    COALESCE(tel.network_logs, '[]'::jsonb),
    COALESCE(tel.device_meta, '{}'::jsonb)
FROM feedback_tickets t
LEFT JOIN LATERAL (
    SELECT attempt_number, guardrail_status, pr_number, pr_url,
           test_output, human_review_note, tokens_used
    FROM agent_runs ar
    WHERE ar.ticket_id = t.id
    ORDER BY ar.created_at DESC, ar.id DESC
    LIMIT 1
) r ON TRUE
LEFT JOIN LATERAL (
    SELECT breadcrumbs, console_errors, network_logs, device_meta
    FROM ticket_telemetry tt
    WHERE tt.ticket_id = t.id
    ORDER BY tt.id DESC
    LIMIT 1
) tel ON TRUE
WHERE ($1 = '' OR t.status = $1)
ORDER BY t.updated_at DESC, t.id DESC
LIMIT $2 OFFSET $3`

	rows, err := r.pool.Query(ctx, query, status, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list tickets: %w", err)
	}
	defer rows.Close()

	items := make([]TicketListItem, 0)
	for rows.Next() {
		var item TicketListItem
		if err := rows.Scan(
			&item.ID,
			&item.UserMessage,
			&item.TechnicalSummary,
			&item.AppCommitSHA,
			&item.ModuleTag,
			&item.OccurrenceCount,
			&item.Priority,
			&item.Status,
			&item.MaxDiffOverride,
			&item.CreatedAt,
			&item.UpdatedAt,
			&item.AttemptNumber,
			&item.GuardrailStatus,
			&item.PRNumber,
			&item.PRURL,
			&item.TestOutput,
			&item.HumanReviewNote,
			&item.TokensUsed,
			&item.Breadcrumbs,
			&item.ConsoleErrors,
			&item.NetworkLogs,
			&item.DeviceMeta,
		); err != nil {
			return nil, 0, fmt.Errorf("scan ticket list item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate tickets: %w", err)
	}
	return items, total, nil
}

// GetDashboardStats computes ticket summary counts and merge rate percentage.
func (r *Repository) GetDashboardStats(ctx context.Context) (*DashboardStats, error) {
	const query = `
SELECT
    COUNT(*)::int AS total_tickets,
    COUNT(*) FILTER (WHERE status = 'QUEUED')::int AS queued,
    COUNT(*) FILTER (WHERE status = 'RUNNING')::int AS running,
    COUNT(*) FILTER (WHERE status = 'PR_OPENED')::int AS pr_opened,
    COUNT(*) FILTER (WHERE status = 'MERGED')::int AS merged,
    COUNT(*) FILTER (WHERE status = 'ABORTED')::int AS aborted,
    COUNT(*) FILTER (WHERE status = 'IGNORED_OUTDATED')::int AS ignored_outdated,
    COUNT(*) FILTER (WHERE status = 'FEATURE_REQUEST')::int AS feature_request
FROM feedback_tickets`

	var stats DashboardStats
	if err := r.pool.QueryRow(ctx, query).Scan(
		&stats.TotalTickets,
		&stats.Queued,
		&stats.Running,
		&stats.PROpened,
		&stats.Merged,
		&stats.Aborted,
		&stats.IgnoredOutdated,
		&stats.FeatureRequest,
	); err != nil {
		return nil, fmt.Errorf("query dashboard stats: %w", err)
	}
	if stats.TotalTickets > 0 {
		stats.MergedRatePercent = math.Round((float64(stats.Merged)/float64(stats.TotalTickets))*1000) / 10
	}
	return &stats, nil
}

// RetryTicket requeues an ABORTED or IGNORED_OUTDATED ticket and optionally updates max_diff_override.
func (r *Repository) RetryTicket(ctx context.Context, ticketID int64, maxDiffOverride *int) (*Ticket, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin retry ticket tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM feedback_tickets WHERE id = $1 FOR UPDATE`, ticketID).Scan(&currentStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup ticket %d for retry: %w", ticketID, err)
	}
	if currentStatus != "ABORTED" && currentStatus != "IGNORED_OUTDATED" {
		return nil, fmt.Errorf("%w: ticket %d has status %s", ErrInvalidTransition, ticketID, currentStatus)
	}

	const updateSQL = `
UPDATE feedback_tickets
SET status = 'QUEUED',
    max_diff_override = COALESCE($2, max_diff_override),
    updated_at = NOW()
WHERE id = $1
RETURNING id, user_message, technical_summary, app_commit_sha,
          module_tag, occurrence_count, priority, status,
          max_diff_override, created_at, updated_at`

	var updated Ticket
	err = tx.QueryRow(ctx, updateSQL, ticketID, maxDiffOverride).Scan(
		&updated.ID,
		&updated.UserMessage,
		&updated.TechnicalSummary,
		&updated.AppCommitSHA,
		&updated.ModuleTag,
		&updated.OccurrenceCount,
		&updated.Priority,
		&updated.Status,
		&updated.MaxDiffOverride,
		&updated.CreatedAt,
		&updated.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("retry ticket %d: %w", ticketID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit retry ticket tx: %w", err)
	}
	return &updated, nil
}

// PatchTicket manually updates a ticket's status and/or priority.
func (r *Repository) PatchTicket(ctx context.Context, ticketID int64, params TicketUpdateParams) (*Ticket, error) {
	const updateSQL = `
UPDATE feedback_tickets
SET status = COALESCE($2, status),
    priority = COALESCE($3, priority),
    updated_at = NOW()
WHERE id = $1
RETURNING id, user_message, technical_summary, app_commit_sha,
          module_tag, occurrence_count, priority, status,
          max_diff_override, created_at, updated_at`

	var updated Ticket
	err := r.pool.QueryRow(ctx, updateSQL, ticketID, params.Status, params.Priority).Scan(
		&updated.ID,
		&updated.UserMessage,
		&updated.TechnicalSummary,
		&updated.AppCommitSHA,
		&updated.ModuleTag,
		&updated.OccurrenceCount,
		&updated.Priority,
		&updated.Status,
		&updated.MaxDiffOverride,
		&updated.CreatedAt,
		&updated.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("patch ticket %d: %w", ticketID, err)
	}
	return &updated, nil
}

func loadTicketExtras(ctx context.Context, tx pgx.Tx, ct *ClaimedTicket) error {
	err := tx.QueryRow(ctx, `
SELECT breadcrumbs, console_errors, network_logs, device_meta
FROM ticket_telemetry
WHERE ticket_id = $1
ORDER BY id DESC
LIMIT 1`, ct.Ticket.ID).Scan(
		&ct.Telemetry.Breadcrumbs,
		&ct.Telemetry.ConsoleErrors,
		&ct.Telemetry.NetworkLogs,
		&ct.Telemetry.DeviceMeta,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		ct.Telemetry = Telemetry{
			Breadcrumbs:   json.RawMessage("[]"),
			ConsoleErrors: json.RawMessage("[]"),
			NetworkLogs:   json.RawMessage("[]"),
			DeviceMeta:    json.RawMessage("{}"),
		}
	} else if err != nil {
		return fmt.Errorf("load ticket telemetry: %w", err)
	}

	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM agent_runs WHERE ticket_id = $1`,
		ct.Ticket.ID,
	).Scan(&ct.AttemptNumber); err != nil {
		return fmt.Errorf("compute next attempt number: %w", err)
	}

	rows, err := tx.Query(ctx, `
SELECT ar.human_review_note
FROM agent_runs ar
JOIN feedback_tickets ft ON ft.id = ar.ticket_id
WHERE ft.module_tag = $1
  AND ar.human_review_note <> ''
ORDER BY ar.created_at DESC
LIMIT 5`, ct.Ticket.ModuleTag)
	if err != nil {
		return fmt.Errorf("load past review notes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return fmt.Errorf("scan past review note: %w", err)
		}
		ct.PastReviewNotes = append(ct.PastReviewNotes, note)
	}
	return rows.Err()
}

func normalizeJSON(raw json.RawMessage, fallback string) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return fallback
	}
	return trimmed
}
