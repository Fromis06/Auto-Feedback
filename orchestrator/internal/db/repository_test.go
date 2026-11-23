package db

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
)

func openTestRepo(t *testing.T) *Repository {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dsn == "" {
		if os.Getenv("REQUIRE_DB_TESTS") == "1" {
			t.Fatal("REQUIRE_DB_TESTS=1 but neither TEST_DATABASE_URL nor DATABASE_URL is set")
		}
		t.Skip("skipping PostgreSQL integration test: TEST_DATABASE_URL / DATABASE_URL not set")
	}

	ctx := context.Background()
	repo, err := NewRepository(ctx, dsn)
	if err != nil {
		if os.Getenv("REQUIRE_DB_TESTS") == "1" {
			t.Fatalf("REQUIRE_DB_TESTS=1 and DB connection failed: %v", err)
		}
		t.Skipf("skipping PostgreSQL integration test (DB unreachable): %v", err)
	}
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(context.Background(), "TRUNCATE agent_runs, ticket_telemetry, feedback_tickets RESTART IDENTITY CASCADE")
		repo.Close()
	})
	if _, err := repo.pool.Exec(ctx, "TRUNCATE agent_runs, ticket_telemetry, feedback_tickets RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	return repo
}

func makeUnitVector(dim int, index int) []float32 {
	v := make([]float32, dim)
	v[index] = 1.0
	return v
}

func TestFormatVector(t *testing.T) {
	got := FormatVector([]float32{0.5, -1.25, 0})
	if got != "[0.5,-1.25,0]" {
		t.Fatalf("unexpected FormatVector output: %q", got)
	}
}

func TestIsSimilarityMatchGuardsNaN(t *testing.T) {
	if isSimilarityMatch(math.NaN(), 0.85) {
		t.Fatalf("expected NaN similarity to return false")
	}
	if isSimilarityMatch(0.85, 0.85) {
		t.Fatalf("expected boundary similarity <= threshold to return false")
	}
	if !isSimilarityMatch(0.86, 0.85) {
		t.Fatalf("expected similarity > threshold to return true")
	}
}

func TestPostgresDeduplicationAndPriorityEscalation(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()

	vec := makeUnitVector(768, 0)
	id, err := repo.CreateTicketWithTelemetry(ctx, CreateTicketParams{
		UserMessage:      "Checkout crashes on submit",
		TechnicalSummary: "Null pointer in checkout submit handler",
		Embedding:        vec,
		ModuleTag:        "checkout",
		Priority:         "MEDIUM",
		Status:           "QUEUED",
		Telemetry: Telemetry{
			Breadcrumbs:   json.RawMessage(`[{"action":"click"}]`),
			ConsoleErrors: json.RawMessage(`["TypeError: Cannot read properties of null"]`),
			NetworkLogs:   json.RawMessage(`[{"status":500}]`),
			DeviceMeta:    json.RawMessage(`{"os":"Linux"}`),
		},
	})
	if err != nil {
		t.Fatalf("CreateTicketWithTelemetry: %v", err)
	}

	match, err := repo.FindSimilarOpenTicket(ctx, vec, 0.85)
	if err != nil {
		t.Fatalf("FindSimilarOpenTicket: %v", err)
	}
	if match == nil || match.ID != id || match.Similarity < 0.99 {
		t.Fatalf("expected match on ticket %d, got %+v", id, match)
	}

	// Escalate: 2 -> MEDIUM, 3 -> HIGH, 4 -> HIGH, 5 -> CRITICAL
	expected := []struct {
		count    int
		priority string
	}{
		{2, "MEDIUM"},
		{3, "HIGH"},
		{4, "HIGH"},
		{5, "CRITICAL"},
	}
	for _, exp := range expected {
		c, p, err := repo.IncrementTicketOccurrence(ctx, id)
		if err != nil {
			t.Fatalf("IncrementTicketOccurrence: %v", err)
		}
		if c != exp.count || p != exp.priority {
			t.Fatalf("expected (%d, %s), got (%d, %s)", exp.count, exp.priority, c, p)
		}
	}

	// Orthogonal vector must NOT match.
	otherVec := makeUnitVector(768, 1)
	noMatch, err := repo.FindSimilarOpenTicket(ctx, otherVec, 0.85)
	if err != nil {
		t.Fatalf("FindSimilarOpenTicket orthogonal: %v", err)
	}
	if noMatch != nil {
		t.Fatalf("expected no match for orthogonal vector, got %+v", noMatch)
	}
}

func TestPostgresModuleConcurrencyLockAndSkipLocked(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()

	// Create two tickets in module "auth" and one in module "billing".
	auth1, err := repo.CreateTicketWithTelemetry(ctx, CreateTicketParams{
		UserMessage: "Auth bug 1",
		ModuleTag:   "auth",
		Priority:    "HIGH",
		Status:      "QUEUED",
	})
	if err != nil {
		t.Fatalf("create auth1: %v", err)
	}
	auth2, err := repo.CreateTicketWithTelemetry(ctx, CreateTicketParams{
		UserMessage: "Auth bug 2",
		ModuleTag:   "auth",
		Priority:    "MEDIUM",
		Status:      "QUEUED",
	})
	if err != nil {
		t.Fatalf("create auth2: %v", err)
	}
	billing1, err := repo.CreateTicketWithTelemetry(ctx, CreateTicketParams{
		UserMessage: "Billing bug 1",
		ModuleTag:   "billing",
		Priority:    "MEDIUM",
		Status:      "QUEUED",
	})
	if err != nil {
		t.Fatalf("create billing1: %v", err)
	}

	// Concurrent claims must claim at most ONE auth ticket and the ONE billing ticket.
	var wg sync.WaitGroup
	claimedCh := make(chan *ClaimedTicket, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ct, err := repo.ClaimNextQueuedTicket(ctx)
			if err != nil {
				t.Errorf("ClaimNextQueuedTicket error: %v", err)
				return
			}
			if ct != nil {
				claimedCh <- ct
			}
		}()
	}
	wg.Wait()
	close(claimedCh)

	claimedIDs := map[int64]string{}
	for ct := range claimedCh {
		claimedIDs[ct.Ticket.ID] = ct.Ticket.ModuleTag
	}
	if len(claimedIDs) != 2 {
		t.Fatalf("expected 2 claimed tickets (1 auth + 1 billing), got %v", claimedIDs)
	}
	if _, ok := claimedIDs[auth1]; !ok {
		t.Fatalf("expected auth1 (%d) to be claimed, got %v", auth1, claimedIDs)
	}
	if _, ok := claimedIDs[billing1]; !ok {
		t.Fatalf("expected billing1 (%d) to be claimed, got %v", billing1, claimedIDs)
	}
	if _, ok := claimedIDs[auth2]; ok {
		t.Fatalf("auth2 (%d) should NOT be claimed while auth1 is RUNNING", auth2)
	}

	// Record PR opened for auth1; auth2 must STILL be blocked while auth1 is PR_OPENED.
	if err := repo.UpdateTicketStatus(ctx, auth1, "PR_OPENED"); err != nil {
		t.Fatalf("update auth1 PR_OPENED: %v", err)
	}
	ct, err := repo.ClaimNextQueuedTicket(ctx)
	if err != nil {
		t.Fatalf("claim while PR_OPENED: %v", err)
	}
	if ct != nil {
		t.Fatalf("expected nil claim while auth1 is PR_OPENED, got %+v", ct)
	}

	// Simulate PR rejection on auth1 with review note; auth2 becomes claimable and receives the note!
	prNum := 42
	if _, err := repo.RecordAgentRun(ctx, AgentRunParams{
		TicketID:        auth1,
		AttemptNumber:   1,
		GuardrailStatus: "PASSED",
		PRNumber:        &prNum,
		PRURL:           "https://github.com/org/repo/pull/42",
	}); err != nil {
		t.Fatalf("RecordAgentRun: %v", err)
	}
	if err := repo.RecordPRReviewRejection(ctx, prNum, 0, "Do not bypass CSRF middleware"); err != nil {
		t.Fatalf("RecordPRReviewRejection: %v", err)
	}

	ct2, err := repo.ClaimNextQueuedTicket(ctx)
	if err != nil {
		t.Fatalf("claim auth2 after auth1 aborted: %v", err)
	}
	if ct2 == nil || ct2.Ticket.ID != auth2 {
		t.Fatalf("expected auth2 (%d) to be claimed, got %+v", auth2, ct2)
	}
	if len(ct2.PastReviewNotes) != 1 || ct2.PastReviewNotes[0] != "Do not bypass CSRF middleware" {
		t.Fatalf("expected past review note propagated, got %+v", ct2.PastReviewNotes)
	}
}

func TestPostgresAdminQueriesRetryAndPatch(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()

	if err := repo.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	abortedID, err := repo.CreateTicketWithTelemetry(ctx, CreateTicketParams{
		UserMessage:      "Complex bug",
		TechnicalSummary: "Refactor needed",
		ModuleTag:        "payments",
		Priority:         "HIGH",
		Status:           "ABORTED",
		Telemetry: Telemetry{
			Breadcrumbs:   json.RawMessage(`[{"type":"click"}]`),
			ConsoleErrors: json.RawMessage(`["RangeError"]`),
			NetworkLogs:   json.RawMessage(`[{"status":500}]`),
			DeviceMeta:    json.RawMessage(`{"os":"Linux"}`),
		},
	})
	if err != nil {
		t.Fatalf("create aborted ticket: %v", err)
	}
	if _, err := repo.RecordAgentRun(ctx, AgentRunParams{
		TicketID:        abortedID,
		AttemptNumber:   1,
		GuardrailStatus: "BLOCKED_MAX_DIFF",
		TestOutput:      "diff too large",
		TokensUsed:      900,
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}

	featID, err := repo.CreateTicketWithTelemetry(ctx, CreateTicketParams{
		UserMessage: "Add CSV export",
		ModuleTag:   "reports",
		Priority:    "LOW",
		Status:      "FEATURE_REQUEST",
	})
	if err != nil {
		t.Fatalf("create feature request: %v", err)
	}

	// ListTickets joins agent_runs and ticket_telemetry
	items, total, err := repo.ListTickets(ctx, TicketListFilter{Status: "ABORTED", Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].GuardrailStatus != "BLOCKED_MAX_DIFF" || items[0].TokensUsed != 900 {
		t.Fatalf("unexpected ListTickets result: total=%d items=%+v", total, items)
	}

	// Stats
	stats, err := repo.GetDashboardStats(ctx)
	if err != nil {
		t.Fatalf("GetDashboardStats: %v", err)
	}
	if stats.TotalTickets != 2 || stats.Aborted != 1 || stats.FeatureRequest != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	// Retry with max_diff_override=500 and verify ClaimNextQueuedTicket reads it back
	override := 500
	retried, err := repo.RetryTicket(ctx, abortedID, &override)
	if err != nil {
		t.Fatalf("RetryTicket: %v", err)
	}
	if retried.Status != "QUEUED" || retried.MaxDiffOverride == nil || *retried.MaxDiffOverride != 500 {
		t.Fatalf("unexpected retried ticket: %+v", retried)
	}
	claimed, err := repo.ClaimNextQueuedTicket(ctx)
	if err != nil || claimed == nil || claimed.Ticket.ID != abortedID {
		t.Fatalf("expected retried ticket claimed, got %+v err=%v", claimed, err)
	}
	if claimed.Ticket.MaxDiffOverride == nil || *claimed.Ticket.MaxDiffOverride != 500 {
		t.Fatalf("expected claimed ticket MaxDiffOverride=500, got %v", claimed.Ticket.MaxDiffOverride)
	}

	// Patch FEATURE_REQUEST -> QUEUED + priority NORMAL
	newStatus := "QUEUED"
	newPriority := "NORMAL"
	patched, err := repo.PatchTicket(ctx, featID, TicketUpdateParams{Status: &newStatus, Priority: &newPriority})
	if err != nil {
		t.Fatalf("PatchTicket: %v", err)
	}
	if patched.Status != "QUEUED" || patched.Priority != "NORMAL" {
		t.Fatalf("unexpected patched ticket: %+v", patched)
	}
}
