package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	keys := []string{
		"PORT", "ORCHESTRATOR_PORT", "DATABASE_URL",
		"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_DB",
		"TARGET_REPO_URL", "GITHUB_TOKEN", "GITHUB_WEBHOOK_SECRET", "GITHUB_API_URL",
		"BASE_BRANCH", "LLM_BASE_URL", "LLM_API_KEY", "EMBEDDING_MODEL", "VECTOR_DIM",
		"TRIAGE_MODEL", "CODING_MODEL", "SIMILARITY_THRESHOLD", "PROTECTED_PATHS",
		"MAX_DIFF_LINES", "TEST_COMMAND", "SANDBOX_IMAGE", "CORS_ALLOWED_ORIGIN",
		"ADMIN_USER", "ADMIN_PASSWORD", "ALERT_WEBHOOK_URL", "PUBLIC_DASHBOARD_URL",
		"WORKER_COUNT", "WORKER_POLL_INTERVAL", "SANDBOX_TIMEOUT",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Port != "8080" {
		t.Fatalf("expected Port 8080, got %q", cfg.Port)
	}
	expectedDB := "postgres://autopatch:autopatch@db:5432/autopatch?sslmode=disable"
	if cfg.DatabaseURL != expectedDB {
		t.Fatalf("expected DatabaseURL %q, got %q", expectedDB, cfg.DatabaseURL)
	}
	if cfg.VectorDim != 768 {
		t.Fatalf("expected VectorDim 768, got %d", cfg.VectorDim)
	}
	if cfg.SimilarityThreshold != 0.85 {
		t.Fatalf("expected SimilarityThreshold 0.85, got %v", cfg.SimilarityThreshold)
	}
	if cfg.MaxDiffLines != 200 {
		t.Fatalf("expected MaxDiffLines 200, got %d", cfg.MaxDiffLines)
	}
	if cfg.SandboxImage != "autopatch-sandbox:latest" {
		t.Fatalf("expected SandboxImage autopatch-sandbox:latest, got %q", cfg.SandboxImage)
	}
	if cfg.WorkerPollInterval != 3*time.Second {
		t.Fatalf("expected WorkerPollInterval 3s, got %v", cfg.WorkerPollInterval)
	}
	if cfg.CORSAllowedOrigin != "*" {
		t.Fatalf("expected default CORSAllowedOrigin *, got %q", cfg.CORSAllowedOrigin)
	}
	if cfg.EmbeddingModel != "auto" || cfg.TriageModel != "auto" || cfg.CodingModel != "auto" {
		t.Fatalf("expected auto defaults for models, got emb=%q triage=%q coding=%q", cfg.EmbeddingModel, cfg.TriageModel, cfg.CodingModel)
	}
	if cfg.AdminUser != "admin" || cfg.AdminPassword != "" {
		t.Fatalf("unexpected default admin config: user=%q pass=%q", cfg.AdminUser, cfg.AdminPassword)
	}
	if cfg.AlertWebhookURL != "" || cfg.PublicDashboardURL != "" {
		t.Fatalf("expected empty alert/dashboard defaults, got %q / %q", cfg.AlertWebhookURL, cfg.PublicDashboardURL)
	}
}

func TestNormalizeLLMBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://api.openai.com/v1/", "https://api.openai.com/v1"},
		{"https://api.openai.com", "https://api.openai.com/v1"},
		{"https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"https://generativelanguage.googleapis.com/", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"https://generativelanguage.googleapis.com/v1beta", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"https://generativelanguage.googleapis.com/v1beta/openai/", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"http://host.docker.internal:11434", "http://host.docker.internal:11434/v1"},
		{"http://localhost:11434/", "http://localhost:11434/v1"},
		{"http://localhost:11434/v1/", "http://localhost:11434/v1"},
		{"http://host.docker.internal:8000", "http://host.docker.internal:8000/v1"},
		{"http://127.0.0.1:54321/", "http://127.0.0.1:54321"},
	}
	for _, tc := range cases {
		if got := NormalizeLLMBaseURL(tc.in); got != tc.want {
			t.Fatalf("NormalizeLLMBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLoadOverridesAndValidation(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("ORCHESTRATOR_PORT", "9090")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5433/custom?sslmode=disable")
	t.Setenv("VECTOR_DIM", "768")
	t.Setenv("SIMILARITY_THRESHOLD", "0.9")
	t.Setenv("MAX_DIFF_LINES", "150")
	t.Setenv("LLM_BASE_URL", "http://localhost:11434/v1/")
	t.Setenv("GITHUB_API_URL", "https://github.example.com/api/v3/")
	t.Setenv("CORS_ALLOWED_ORIGIN", "https://app.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Port != "9090" {
		t.Fatalf("expected Port 9090, got %q", cfg.Port)
	}
	if cfg.LLMBaseURL != "http://localhost:11434/v1" {
		t.Fatalf("expected trimmed LLMBaseURL, got %q", cfg.LLMBaseURL)
	}
	if cfg.GitHubAPIURL != "https://github.example.com/api/v3" {
		t.Fatalf("expected trimmed GitHubAPIURL, got %q", cfg.GitHubAPIURL)
	}
	if cfg.CORSAllowedOrigin != "https://app.example.com" {
		t.Fatalf("expected CORSAllowedOrigin https://app.example.com, got %q", cfg.CORSAllowedOrigin)
	}

	t.Setenv("SIMILARITY_THRESHOLD", "1.5")
	if _, err := Load(); err == nil {
		t.Fatalf("expected error for SIMILARITY_THRESHOLD=1.5")
	}
}
