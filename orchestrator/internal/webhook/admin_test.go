package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autopatch-engine/orchestrator/internal/config"
	"github.com/autopatch-engine/orchestrator/internal/db"
	"github.com/autopatch-engine/orchestrator/internal/llm"
)

func TestAdminAuthMiddlewareAndPublicFeedback(t *testing.T) {
	llmSrv := newMockLLMServer(t, "BUG_FIX", "checkout")
	defer llmSrv.Close()

	store := newMemoryStore()
	_, _ = store.CreateTicketWithTelemetry(context.Background(), db.CreateTicketParams{
		UserMessage:      "Save throws 500",
		TechnicalSummary: "NPE on checkout save",
		ModuleTag:        "checkout",
		Priority:         "HIGH",
		Status:           "ABORTED",
	})

	cfg := &config.Config{
		SimilarityThreshold: 0.85,
		VectorDim:           768,
		AdminUser:           "opsadmin",
		AdminPassword:       "s3cr3t-pass!",
	}
	h := NewHandler(cfg, store, llm.NewClient(llmSrv.URL, "k", "t", "c", "e", 768), nil, nil, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	protectedPaths := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/dashboard", ""},
		{http.MethodGet, "/api/v1/tickets", ""},
		{http.MethodGet, "/api/v1/stats", ""},
		{http.MethodPost, "/api/v1/tickets/1/retry", `{}`},
		{http.MethodPatch, "/api/v1/tickets/1", `{"priority":"CRITICAL"}`},
	}

	// 1. Missing credentials -> 401 on all protected routes
	for _, tc := range protectedPaths {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without auth: expected 401, got %d", tc.method, tc.path, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
			t.Fatalf("%s %s expected WWW-Authenticate Basic header, got %q", tc.method, tc.path, got)
		}
	}

	// 2. Wrong password -> 401
	badReq := httptest.NewRequest(http.MethodGet, "/api/v1/tickets", nil)
	badReq.SetBasicAuth("opsadmin", "wrong-password")
	badRec := httptest.NewRecorder()
	mux.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong basic password, got %d", badRec.Code)
	}

	badBearer := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	badBearer.Header.Set("Authorization", "Bearer nope")
	badBearerRec := httptest.NewRecorder()
	mux.ServeHTTP(badBearerRec, badBearer)
	if badBearerRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong bearer token, got %d", badBearerRec.Code)
	}

	// 3. Valid HTTP Basic Auth -> 200 on /dashboard & /api/v1/tickets
	dashReq := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	dashReq.SetBasicAuth("opsadmin", "s3cr3t-pass!")
	dashRec := httptest.NewRecorder()
	mux.ServeHTTP(dashRec, dashReq)
	if dashRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /dashboard with Basic Auth, got %d", dashRec.Code)
	}
	if !strings.Contains(dashRec.Body.String(), "autopatch-engine") || !strings.Contains(dashRec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("expected embedded HTML dashboard response")
	}

	// 4. Valid Bearer token -> 200 on /api/v1/stats
	statsReq := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	statsReq.Header.Set("Authorization", "Bearer s3cr3t-pass!")
	statsRec := httptest.NewRecorder()
	mux.ServeHTTP(statsRec, statsReq)
	if statsRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/v1/stats with Bearer token, got %d", statsRec.Code)
	}

	// 5. Public POST /api/v1/feedback MUST still work without Admin Auth!
	fbReq := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", strings.NewReader(`{"user_message":"Checkout crashes"}`))
	fbRec := httptest.NewRecorder()
	mux.ServeHTTP(fbRec, fbReq)
	if fbRec.Code != http.StatusAccepted {
		t.Fatalf("expected public POST /api/v1/feedback to return 202 without admin auth, got %d: %s", fbRec.Code, fbRec.Body.String())
	}
}

func TestAdminTicketsStatsRetryAndPatch(t *testing.T) {
	store := newMemoryStore()
	ctx := context.Background()

	// Seed 4 tickets across statuses + telemetry + agent run
	t1, _ := store.CreateTicketWithTelemetry(ctx, db.CreateTicketParams{
		UserMessage:      "Checkout crashes on save",
		TechnicalSummary: "Nil dereference in checkout",
		AppCommitSHA:     "abc1234",
		ModuleTag:        "checkout",
		Priority:         "HIGH",
		Status:           "ABORTED",
		Telemetry: db.Telemetry{
			Breadcrumbs:   json.RawMessage(`[{"type":"click","target":"#save"}]`),
			ConsoleErrors: json.RawMessage(`["TypeError: undefined"]`),
			NetworkLogs:   json.RawMessage(`[{"status":500,"url":"/api/save"}]`),
			DeviceMeta:    json.RawMessage(`{"os":"macOS"}`),
		},
	})
	_, _ = store.RecordAgentRun(ctx, db.AgentRunParams{
		TicketID:        t1,
		AttemptNumber:   1,
		GuardrailStatus: "BLOCKED_MAX_DIFF",
		TestOutput:      "diff exceeded 200 lines (got 340)",
		HumanReviewNote: "Keep changes scoped",
		TokensUsed:      1200,
	})

	t2, _ := store.CreateTicketWithTelemetry(ctx, db.CreateTicketParams{
		UserMessage:      "Please add dark mode toggle",
		TechnicalSummary: "Add dark mode setting",
		ModuleTag:        "ui",
		Priority:         "LOW",
		Status:           "FEATURE_REQUEST",
	})

	prNum := 42
	t3, _ := store.CreateTicketWithTelemetry(ctx, db.CreateTicketParams{
		UserMessage: "Login button broken",
		ModuleTag:   "auth",
		Priority:    "MEDIUM",
		Status:      "MERGED",
	})
	_, _ = store.RecordAgentRun(ctx, db.AgentRunParams{
		TicketID:        t3,
		AttemptNumber:   1,
		GuardrailStatus: "PASSED",
		PRNumber:        &prNum,
		PRURL:           "https://github.com/acme/app/pull/42",
	})

	_, _ = store.CreateTicketWithTelemetry(ctx, db.CreateTicketParams{
		UserMessage: "Old bug report",
		ModuleTag:   "billing",
		Priority:    "LOW",
		Status:      "IGNORED_OUTDATED",
	})

	cfg := &config.Config{AdminUser: "admin"} // Empty password -> open for direct admin tests
	h := NewHandler(cfg, store, nil, nil, nil, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// 1. GET /api/v1/stats
	statsRec := httptest.NewRecorder()
	mux.ServeHTTP(statsRec, httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil))
	if statsRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/stats expected 200, got %d", statsRec.Code)
	}
	var stats db.DashboardStats
	if err := json.NewDecoder(statsRec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.TotalTickets != 4 || stats.Merged != 1 || stats.Aborted != 1 || stats.FeatureRequest != 1 || stats.MergedRatePercent != 25.0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	// 2. GET /api/v1/tickets?status=ABORTED&limit=10&offset=0
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/api/v1/tickets?status=ABORTED&limit=10&offset=0", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/tickets expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	var listResp struct {
		Tickets []db.TicketListItem `json:"tickets"`
		Total   int                 `json:"total"`
		Limit   int                 `json:"limit"`
		Offset  int                 `json:"offset"`
	}
	if err := json.NewDecoder(listRec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode tickets list: %v", err)
	}
	if listResp.Total != 1 || len(listResp.Tickets) != 1 {
		t.Fatalf("expected 1 ABORTED ticket, got total=%d len=%d", listResp.Total, len(listResp.Tickets))
	}
	item := listResp.Tickets[0]
	if item.ID != t1 || item.GuardrailStatus != "BLOCKED_MAX_DIFF" || item.TokensUsed != 1200 || !strings.Contains(string(item.Breadcrumbs), "#save") {
		t.Fatalf("unexpected joined ticket item: %+v", item)
	}

	// 3. POST /api/v1/tickets/{id}/retry with max_diff_override=500
	retryReq := httptest.NewRequest(http.MethodPost, "/api/v1/tickets/1/retry", strings.NewReader(`{"max_diff_override":500}`))
	retryRec := httptest.NewRecorder()
	mux.ServeHTTP(retryRec, retryReq)
	if retryRec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/tickets/1/retry expected 200, got %d: %s", retryRec.Code, retryRec.Body.String())
	}
	var retried db.Ticket
	if err := json.NewDecoder(retryRec.Body).Decode(&retried); err != nil {
		t.Fatalf("decode retried ticket: %v", err)
	}
	if retried.Status != "QUEUED" {
		t.Fatalf("expected status QUEUED after retry, got %q", retried.Status)
	}
	if retried.MaxDiffOverride == nil || *retried.MaxDiffOverride != 500 {
		t.Fatalf("expected max_diff_override=500 persisted, got %v", retried.MaxDiffOverride)
	}

	// Retrying a MERGED ticket -> 409 Conflict
	conflictRec := httptest.NewRecorder()
	mux.ServeHTTP(conflictRec, httptest.NewRequest(http.MethodPost, "/api/v1/tickets/3/retry", strings.NewReader(`{}`)))
	if conflictRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 retrying MERGED ticket, got %d", conflictRec.Code)
	}

	// 4. PATCH /api/v1/tickets/{id}: promote FEATURE_REQUEST -> QUEUED + priority NORMAL
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/v1/tickets/2", strings.NewReader(`{"status":"QUEUED","priority":"NORMAL"}`))
	patchRec := httptest.NewRecorder()
	mux.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH /api/v1/tickets/2 expected 200, got %d: %s", patchRec.Code, patchRec.Body.String())
	}
	var patched db.Ticket
	if err := json.NewDecoder(patchRec.Body).Decode(&patched); err != nil {
		t.Fatalf("decode patched ticket: %v", err)
	}
	if patched.ID != t2 || patched.Status != "QUEUED" || patched.Priority != "NORMAL" {
		t.Fatalf("unexpected patched ticket: %+v", patched)
	}

	// Invalid status on PATCH -> 400
	badPatchRec := httptest.NewRecorder()
	mux.ServeHTTP(badPatchRec, httptest.NewRequest(http.MethodPatch, "/api/v1/tickets/2", strings.NewReader(`{"status":"BOGUS"}`)))
	if badPatchRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid status patch, got %d", badPatchRec.Code)
	}
}
