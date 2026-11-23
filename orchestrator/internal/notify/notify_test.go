package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestWebhookNotifierDiscordPayloads(t *testing.T) {
	var mu sync.Mutex
	var bodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("expected application/json Content-Type, got %q", ct)
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Include "discord.com/api/webhooks" in path so Discord payload formatting activates against httptest.
	discordURL := srv.URL + "/discord.com/api/webhooks/12345/secret-token"
	notifier := NewWebhookNotifier(discordURL, "https://autopatch.example.com/dashboard", nil)

	// 1. PASSED event
	err := notifier.Send(context.Background(), Event{
		TicketID:        42,
		ModuleTag:       "checkout",
		Summary:         "Fix null total calculation in cart",
		OccurrenceCount: 7,
		GuardrailStatus: "PASSED",
		TicketStatus:    "PR_OPENED",
		PRURL:           "https://github.com/acme/app/pull/88",
	})
	if err != nil {
		t.Fatalf("Send PASSED to Discord failed: %v", err)
	}

	// 2. ABORTED event
	err = notifier.Send(context.Background(), Event{
		TicketID:        43,
		ModuleTag:       "auth",
		Summary:         "Session refresh race condition",
		OccurrenceCount: 2,
		GuardrailStatus: "BLOCKED_MAX_DIFF",
		TicketStatus:    "ABORTED",
	})
	if err != nil {
		t.Fatalf("Send ABORTED to Discord failed: %v", err)
	}

	if len(bodies) != 2 {
		t.Fatalf("expected 2 webhook calls, got %d", len(bodies))
	}

	var passedPayload discordPayload
	if err := json.Unmarshal([]byte(bodies[0]), &passedPayload); err != nil {
		t.Fatalf("decode discord PASSED payload: %v", err)
	}
	if !strings.Contains(passedPayload.Content, "#42") || len(passedPayload.Embeds) != 1 {
		t.Fatalf("unexpected Discord PASSED payload: %+v", passedPayload)
	}
	if passedPayload.Embeds[0].Color != 0x22c55e {
		t.Fatalf("expected green embed color, got %x", passedPayload.Embeds[0].Color)
	}
	joinedPassed := bodies[0]
	for _, want := range []string{"checkout", "7 user(s)", "https://github.com/acme/app/pull/88", "https://autopatch.example.com/dashboard"} {
		if !strings.Contains(joinedPassed, want) {
			t.Fatalf("expected %q in Discord PASSED payload: %s", want, joinedPassed)
		}
	}

	var abortedPayload discordPayload
	if err := json.Unmarshal([]byte(bodies[1]), &abortedPayload); err != nil {
		t.Fatalf("decode discord ABORTED payload: %v", err)
	}
	if abortedPayload.Embeds[0].Color != 0xef4444 {
		t.Fatalf("expected red embed color, got %x", abortedPayload.Embeds[0].Color)
	}
	for _, want := range []string{"#43", "auth", "BLOCKED_MAX_DIFF", "https://autopatch.example.com/dashboard"} {
		if !strings.Contains(bodies[1], want) {
			t.Fatalf("expected %q in Discord ABORTED payload: %s", want, bodies[1])
		}
	}
}

func TestWebhookNotifierSlackPayloadsAndAsync(t *testing.T) {
	var mu sync.Mutex
	var received []slackPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p slackPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode slack payload: %v", err)
		}
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	slackURL := srv.URL + "/services/T000/B000/XXXX"
	notifier := NewWebhookNotifier(slackURL, "https://ops.example.com/dashboard", nil)

	notifier.NotifyAsync(Event{
		TicketID:        9,
		ModuleTag:       "billing",
		Summary:         "Invoice PDF download 500",
		OccurrenceCount: 4,
		GuardrailStatus: "PASSED",
		TicketStatus:    "PR_OPENED",
		PRURL:           "https://github.com/acme/app/pull/99",
	})
	notifier.NotifyAsync(Event{
		TicketID:        10,
		ModuleTag:       "payments",
		Summary:         "Stripe retry timeout",
		OccurrenceCount: 1,
		GuardrailStatus: "TEST_FAILED",
		TicketStatus:    "ABORTED",
	})
	notifier.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("expected 2 async Slack deliveries, got %d", len(received))
	}

	var sawPassed, sawAborted bool
	for _, p := range received {
		if strings.Contains(p.Text, "#9") && strings.Contains(p.Text, "billing") && strings.Contains(p.Text, "https://github.com/acme/app/pull/99") && strings.Contains(p.Text, "https://ops.example.com/dashboard") {
			sawPassed = true
		}
		if strings.Contains(p.Text, "#10") && strings.Contains(p.Text, "payments") && strings.Contains(p.Text, "TEST_FAILED") && strings.Contains(p.Text, "https://ops.example.com/dashboard") {
			sawAborted = true
		}
	}
	if !sawPassed || !sawAborted {
		t.Fatalf("missing expected Slack alerts: sawPassed=%v sawAborted=%v payloads=%+v", sawPassed, sawAborted, received)
	}

	// Empty URL must be a safe no-op
	noop := NewWebhookNotifier("", "", nil)
	noop.NotifyAsync(Event{TicketID: 1, GuardrailStatus: "PASSED"})
	noop.Wait()
	if err := noop.Send(context.Background(), Event{TicketID: 1}); err != nil {
		t.Fatalf("expected nil error for noop notifier, got %v", err)
	}
}
