package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrNotFound is returned when a requested ticket or agent run does not exist.
	ErrNotFound = errors.New("record not found")
	// ErrInvalidTransition is returned when a ticket cannot transition from its current status.
	ErrInvalidTransition = errors.New("invalid ticket state transition")
)

// Ticket mirrors a row in feedback_tickets.
type Ticket struct {
	ID               int64     `json:"id"`
	UserMessage      string    `json:"user_message"`
	TechnicalSummary string    `json:"technical_summary"`
	AppCommitSHA     string    `json:"app_commit_sha"`
	ModuleTag        string    `json:"module_tag"`
	OccurrenceCount  int       `json:"occurrence_count"`
	Priority         string    `json:"priority"`
	Status           string    `json:"status"`
	MaxDiffOverride  *int      `json:"max_diff_override,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Telemetry mirrors a row in ticket_telemetry.
type Telemetry struct {
	Breadcrumbs   json.RawMessage `json:"breadcrumbs"`
	ConsoleErrors json.RawMessage `json:"console_errors"`
	NetworkLogs   json.RawMessage `json:"network_logs"`
	DeviceMeta    json.RawMessage `json:"device_meta"`
}

// ClaimedTicket bundles a claimed QUEUED ticket with its telemetry, next attempt number,
// and past human review notes for the same module_tag.
type ClaimedTicket struct {
	Ticket          Ticket
	Telemetry       Telemetry
	AttemptNumber   int
	PastReviewNotes []string
}

// SimilarTicketMatch holds the closest open ticket matched via pgvector cosine similarity.
type SimilarTicketMatch struct {
	ID              int64
	ModuleTag       string
	OccurrenceCount int
	Priority        string
	Status          string
	Similarity      float64
}

// CreateTicketParams contains the fields needed to create a ticket + telemetry atomically.
type CreateTicketParams struct {
	UserMessage      string
	TechnicalSummary string
	Embedding        []float32
	AppCommitSHA     string
	ModuleTag        string
	Priority         string
	Status           string
	Telemetry        Telemetry
}

// AgentRunParams contains fields for inserting a row into agent_runs.
type AgentRunParams struct {
	TicketID        int64
	AttemptNumber   int
	ModifiedFiles   []string
	TestOutput      string
	GuardrailStatus string
	PRNumber        *int
	PRURL           string
	HumanReviewNote string
	TokensUsed      int
}

// TicketListFilter controls status filtering and pagination for GET /api/v1/tickets.
type TicketListFilter struct {
	Status string
	Limit  int
	Offset int
}

// TicketListItem joins feedback_tickets with its latest agent_run and ticket_telemetry.
type TicketListItem struct {
	ID               int64           `json:"id"`
	UserMessage      string          `json:"user_message"`
	TechnicalSummary string          `json:"technical_summary"`
	AppCommitSHA     string          `json:"app_commit_sha"`
	ModuleTag        string          `json:"module_tag"`
	OccurrenceCount  int             `json:"occurrence_count"`
	Priority         string          `json:"priority"`
	Status           string          `json:"status"`
	MaxDiffOverride  *int            `json:"max_diff_override,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	AttemptNumber    int             `json:"attempt_number"`
	GuardrailStatus  string          `json:"guardrail_status"`
	PRNumber         *int            `json:"pr_number,omitempty"`
	PRURL            string          `json:"pr_url"`
	TestOutput       string          `json:"test_output"`
	HumanReviewNote  string          `json:"human_review_note"`
	TokensUsed       int             `json:"tokens_used"`
	Breadcrumbs      json.RawMessage `json:"breadcrumbs"`
	ConsoleErrors    json.RawMessage `json:"console_errors"`
	NetworkLogs      json.RawMessage `json:"network_logs"`
	DeviceMeta       json.RawMessage `json:"device_meta"`
}

// TicketUpdateParams holds optional manual overrides for PATCH /api/v1/tickets/{id}.
type TicketUpdateParams struct {
	Status   *string
	Priority *string
}

// DashboardStats summarizes ticket lifecycle counts for GET /api/v1/stats.
type DashboardStats struct {
	TotalTickets      int     `json:"total_tickets"`
	Queued            int     `json:"queued"`
	Running           int     `json:"running"`
	PROpened          int     `json:"pr_opened"`
	Merged            int     `json:"merged"`
	Aborted           int     `json:"aborted"`
	IgnoredOutdated   int     `json:"ignored_outdated"`
	FeatureRequest    int     `json:"feature_request"`
	MergedRatePercent float64 `json:"merged_rate_percent"`
}

// Store defines all persistence operations required by the orchestrator.
type Store interface {
	Ping(ctx context.Context) error
	EnsureSchema(ctx context.Context) error
	FindSimilarOpenTicket(ctx context.Context, embedding []float32, threshold float64) (*SimilarTicketMatch, error)
	IncrementTicketOccurrence(ctx context.Context, ticketID int64) (int, string, error)
	CreateTicketWithTelemetry(ctx context.Context, params CreateTicketParams) (int64, error)
	RecordAgentRun(ctx context.Context, params AgentRunParams) (int64, error)
	ClaimNextQueuedTicket(ctx context.Context) (*ClaimedTicket, error)
	UpdateTicketStatus(ctx context.Context, ticketID int64, status string) error
	GetTicketContext(ctx context.Context, ticketID int64) (*ClaimedTicket, error)
	RecordPRReviewRejection(ctx context.Context, prNumber int, fallbackTicketID int64, reviewNote string) error
	MarkTicketMergedByPR(ctx context.Context, prNumber int, fallbackTicketID int64) error
	GetPastReviewNotes(ctx context.Context, moduleTag string, limit int) ([]string, error)
	ListTickets(ctx context.Context, filter TicketListFilter) ([]TicketListItem, int, error)
	GetDashboardStats(ctx context.Context) (*DashboardStats, error)
	RetryTicket(ctx context.Context, ticketID int64, maxDiffOverride *int) (*Ticket, error)
	PatchTicket(ctx context.Context, ticketID int64, params TicketUpdateParams) (*Ticket, error)
}
