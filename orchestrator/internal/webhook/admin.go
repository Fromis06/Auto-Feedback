package webhook

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/autopatch-engine/orchestrator/internal/db"
	"github.com/autopatch-engine/orchestrator/internal/web"
)

var allowedTicketStatuses = map[string]bool{
	"QUEUED":           true,
	"RUNNING":          true,
	"PR_OPENED":        true,
	"MERGED":           true,
	"ABORTED":          true,
	"IGNORED_OUTDATED": true,
	"FEATURE_REQUEST":  true,
}

var allowedTicketPriorities = map[string]bool{
	"LOW":      true,
	"MEDIUM":   true,
	"NORMAL":   true,
	"HIGH":     true,
	"CRITICAL": true,
}

// requireAdminAuth enforces HTTP Basic Auth (ADMIN_USER / ADMIN_PASSWORD) or
// Authorization: Bearer <ADMIN_PASSWORD> whenever ADMIN_PASSWORD is configured.
func (h *Handler) requireAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg == nil || strings.TrimSpace(h.cfg.AdminPassword) == "" {
			next(w, r)
			return
		}

		expectedUser := strings.TrimSpace(h.cfg.AdminUser)
		if expectedUser == "" {
			expectedUser = "admin"
		}
		expectedPass := h.cfg.AdminPassword

		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "Bearer ") {
			token := strings.TrimSpace(authHeader[7:])
			if constantTimeEqual(token, expectedPass) {
				next(w, r)
				return
			}
		}

		if user, pass, ok := r.BasicAuth(); ok {
			userMatch := constantTimeEqual(user, expectedUser)
			passMatch := constantTimeEqual(pass, expectedPass)
			if userMatch && passMatch {
				next(w, r)
				return
			}
		}

		w.Header().Set("WWW-Authenticate", `Basic realm="autopatch-admin"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// HandleDashboard serves the single-file embedded Web Dashboard at GET /dashboard.
func (h *Handler) HandleDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(web.DashboardHTML)
}

// HandleListTickets serves GET /api/v1/tickets with optional ?status=, ?limit=, and ?offset=.
func (h *Handler) HandleListTickets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := strings.ToUpper(strings.TrimSpace(q.Get("status")))
	if status == "ALL" {
		status = ""
	}
	if status != "" && !allowedTicketStatuses[status] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid status filter"})
		return
	}

	limit := 50
	if rawLimit := strings.TrimSpace(q.Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit parameter"})
			return
		}
		if parsed > 200 {
			parsed = 200
		}
		limit = parsed
	}

	offset := 0
	if rawOffset := strings.TrimSpace(q.Get("offset")); rawOffset != "" {
		parsed, err := strconv.Atoi(rawOffset)
		if err != nil || parsed < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid offset parameter"})
			return
		}
		offset = parsed
	}

	tickets, total, err := h.store.ListTickets(r.Context(), db.TicketListFilter{
		Status: status,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		h.logger.Error("list tickets failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	if tickets == nil {
		tickets = []db.TicketListItem{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tickets": tickets,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// HandleStats serves GET /api/v1/stats with overview counts and merge rate percentage.
func (h *Handler) HandleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.GetDashboardStats(r.Context())
	if err != nil {
		h.logger.Error("get dashboard stats failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

type retryTicketRequest struct {
	MaxDiffOverride *int `json:"max_diff_override"`
}

// HandleRetryTicket serves POST /api/v1/tickets/{id}/retry to requeue ABORTED or IGNORED_OUTDATED tickets.
func (h *Handler) HandleRetryTicket(w http.ResponseWriter, r *http.Request) {
	ticketID, err := parseTicketIDPathParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid ticket id"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req retryTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}
	if req.MaxDiffOverride != nil && *req.MaxDiffOverride <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max_diff_override must be positive"})
		return
	}

	updated, err := h.store.RetryTicket(r.Context(), ticketID, req.MaxDiffOverride)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "ticket not found"})
		case errors.Is(err, db.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			h.logger.Error("retry ticket failed", "ticket_id", ticketID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		}
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

type patchTicketRequest struct {
	Status   *string `json:"status"`
	Priority *string `json:"priority"`
}

// HandlePatchTicket serves PATCH /api/v1/tickets/{id} for manual status and priority overrides.
func (h *Handler) HandlePatchTicket(w http.ResponseWriter, r *http.Request) {
	ticketID, err := parseTicketIDPathParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid ticket id"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req patchTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}
	if req.Status == nil && req.Priority == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status or priority is required"})
		return
	}

	var params db.TicketUpdateParams
	if req.Status != nil {
		normalized := strings.ToUpper(strings.TrimSpace(*req.Status))
		if !allowedTicketStatuses[normalized] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid status value"})
			return
		}
		params.Status = &normalized
	}
	if req.Priority != nil {
		normalized := strings.ToUpper(strings.TrimSpace(*req.Priority))
		if !allowedTicketPriorities[normalized] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid priority value"})
			return
		}
		params.Priority = &normalized
	}

	updated, err := h.store.PatchTicket(r.Context(), ticketID, params)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "ticket not found"})
			return
		}
		h.logger.Error("patch ticket failed", "ticket_id", ticketID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

func parseTicketIDPathParam(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}
