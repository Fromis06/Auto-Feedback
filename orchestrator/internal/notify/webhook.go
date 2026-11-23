package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const defaultSendTimeout = 5 * time.Second

// Event carries the sandbox completion context dispatched to ChatOps webhooks.
type Event struct {
	TicketID        int64
	ModuleTag       string
	Summary         string
	OccurrenceCount int
	GuardrailStatus string
	TicketStatus    string
	PRURL           string
	DashboardURL    string
}

// Notifier dispatches sandbox lifecycle alerts without blocking worker goroutines.
type Notifier interface {
	NotifyAsync(event Event)
	Send(ctx context.Context, event Event) error
	Wait()
}

// WebhookNotifier posts formatted notifications to Discord or Slack/Generic webhooks.
type WebhookNotifier struct {
	webhookURL   string
	dashboardURL string
	httpClient   *http.Client
	logger       *slog.Logger
	wg           sync.WaitGroup
}

// NewWebhookNotifier creates a ChatOps notifier. When webhookURL is empty, notifications are no-ops.
func NewWebhookNotifier(webhookURL, publicDashboardURL string, logger *slog.Logger) *WebhookNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookNotifier{
		webhookURL:   strings.TrimSpace(webhookURL),
		dashboardURL: strings.TrimRight(strings.TrimSpace(publicDashboardURL), "/"),
		httpClient: &http.Client{
			Timeout: defaultSendTimeout,
		},
		logger: logger,
	}
}

// NotifyAsync sends an alert in a background goroutine with a short timeout so workers never block.
func (n *WebhookNotifier) NotifyAsync(event Event) {
	if n == nil || n.webhookURL == "" {
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), defaultSendTimeout)
		defer cancel()
		if err := n.Send(ctx, event); err != nil {
			n.logger.Warn("chatops alert delivery failed",
				"ticket_id", event.TicketID,
				"guardrail_status", event.GuardrailStatus,
				"error", err,
			)
		}
	}()
}

// Wait blocks until all in-flight asynchronous alert goroutines complete.
func (n *WebhookNotifier) Wait() {
	if n == nil {
		return
	}
	n.wg.Wait()
}

// Send formats and posts a single ChatOps notification synchronously.
func (n *WebhookNotifier) Send(ctx context.Context, event Event) error {
	if n == nil || n.webhookURL == "" {
		return nil
	}
	if strings.TrimSpace(event.DashboardURL) == "" {
		event.DashboardURL = n.dashboardURL
	}

	payload, err := BuildPayload(n.webhookURL, event)
	if err != nil {
		return fmt.Errorf("build alert payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create alert request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post alert webhook: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("alert webhook returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// IsDiscordWebhook detects Discord webhook URLs to format embeds + content payloads.
func IsDiscordWebhook(webhookURL string) bool {
	low := strings.ToLower(strings.TrimSpace(webhookURL))
	return strings.Contains(low, "discord.com/api/webhooks") ||
		strings.Contains(low, "discordapp.com/api/webhooks")
}

// BuildPayload encodes either a Discord embed payload or a Slack/Generic text payload.
func BuildPayload(webhookURL string, event Event) ([]byte, error) {
	if IsDiscordWebhook(webhookURL) {
		return json.Marshal(buildDiscordPayload(event))
	}
	return json.Marshal(buildSlackPayload(event))
}

type discordPayload struct {
	Content string         `json:"content"`
	Embeds  []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color"`
	Fields      []discordEmbedField `json:"fields"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type slackPayload struct {
	Text string `json:"text"`
}

func buildDiscordPayload(e Event) discordPayload {
	module := nonEmpty(e.ModuleTag, "general")
	summary := truncate(nonEmpty(e.Summary, "No summary provided"), 300)
	occurrences := e.OccurrenceCount
	if occurrences <= 0 {
		occurrences = 1
	}

	if strings.EqualFold(e.GuardrailStatus, "PASSED") {
		fields := []discordEmbedField{
			{Name: "Ticket", Value: fmt.Sprintf("#%d", e.TicketID), Inline: true},
			{Name: "Module", Value: fmt.Sprintf("`%s`", module), Inline: true},
			{Name: "Reports", Value: fmt.Sprintf("%d user(s)", occurrences), Inline: true},
		}
		if e.PRURL != "" {
			fields = append(fields, discordEmbedField{Name: "Pull Request", Value: e.PRURL, Inline: false})
		}
		if e.DashboardURL != "" {
			fields = append(fields, discordEmbedField{Name: "Dashboard", Value: e.DashboardURL, Inline: false})
		}
		return discordPayload{
			Content: fmt.Sprintf("[autopatch] PR opened for Ticket #%d (%s)", e.TicketID, module),
			Embeds: []discordEmbed{
				{
					Title:       fmt.Sprintf("PR Opened — Ticket #%d [%s]", e.TicketID, module),
					Description: summary,
					Color:       0x22c55e,
					Fields:      fields,
				},
			},
		}
	}

	reason := nonEmpty(e.GuardrailStatus, "RUNTIME_ERROR")
	fields := []discordEmbedField{
		{Name: "Ticket", Value: fmt.Sprintf("#%d", e.TicketID), Inline: true},
		{Name: "Module", Value: fmt.Sprintf("`%s`", module), Inline: true},
		{Name: "Reason", Value: fmt.Sprintf("`%s`", reason), Inline: true},
	}
	if e.DashboardURL != "" {
		fields = append(fields, discordEmbedField{Name: "Dashboard", Value: e.DashboardURL, Inline: false})
	}
	return discordPayload{
		Content: fmt.Sprintf("[autopatch] Ticket #%d (%s) ABORTED: %s", e.TicketID, module, reason),
		Embeds: []discordEmbed{
			{
				Title:       fmt.Sprintf("Patch Blocked / Failed — Ticket #%d [%s]", e.TicketID, module),
				Description: summary,
				Color:       0xef4444,
				Fields:      fields,
			},
		},
	}
}

func buildSlackPayload(e Event) slackPayload {
	module := nonEmpty(e.ModuleTag, "general")
	summary := truncate(nonEmpty(e.Summary, "No summary provided"), 300)
	occurrences := e.OccurrenceCount
	if occurrences <= 0 {
		occurrences = 1
	}

	var lines []string
	if strings.EqualFold(e.GuardrailStatus, "PASSED") {
		lines = append(lines,
			fmt.Sprintf("*[autopatch-engine] PR Opened* — Ticket `#%d` (`%s`)", e.TicketID, module),
			fmt.Sprintf("• *Summary:* %s", summary),
			fmt.Sprintf("• *Affected Reports:* %d", occurrences),
		)
		if e.PRURL != "" {
			lines = append(lines, fmt.Sprintf("• *GitHub PR:* %s", e.PRURL))
		}
		if e.DashboardURL != "" {
			lines = append(lines, fmt.Sprintf("• *Dashboard:* %s", e.DashboardURL))
		}
	} else {
		reason := nonEmpty(e.GuardrailStatus, "RUNTIME_ERROR")
		lines = append(lines,
			fmt.Sprintf("*[autopatch-engine] Patch Blocked / Failed (ABORTED)* — Ticket `#%d` (`%s`)", e.TicketID, module),
			fmt.Sprintf("• *Reason:* `%s`", reason),
			fmt.Sprintf("• *Summary:* %s", summary),
		)
		if e.DashboardURL != "" {
			lines = append(lines, fmt.Sprintf("• *Inspect / Retry in Dashboard:* %s", e.DashboardURL))
		}
	}

	return slackPayload{Text: strings.Join(lines, "\n")}
}

func nonEmpty(v, fallback string) string {
	if s := strings.TrimSpace(v); s != "" {
		return s
	}
	return fallback
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}
