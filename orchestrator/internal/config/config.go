package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the orchestrator service.
type Config struct {
	Port                string
	DatabaseURL         string
	TargetRepoURL       string
	GitHubToken         string
	GitHubWebhookSecret string
	GitHubAPIURL        string
	BaseBranch          string
	LLMBaseURL          string
	LLMAPIKey           string
	EmbeddingModel      string
	VectorDim           int
	TriageModel         string
	CodingModel         string
	SimilarityThreshold float64
	ProtectedPaths      string
	MaxDiffLines        int
	TestCommand         string
	SandboxImage        string
	CORSAllowedOrigin   string
	AdminUser           string
	AdminPassword       string
	AlertWebhookURL     string
	PublicDashboardURL  string
	WorkerCount         int
	WorkerPollInterval  time.Duration
	SandboxTimeout      time.Duration
}

// Load reads environment variables and returns a validated Config.
func Load() (*Config, error) {
	port := getEnv("PORT", "")
	if port == "" {
		port = getEnv("ORCHESTRATOR_PORT", "8080")
	}

	dbURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dbURL == "" {
		pgUser := getEnv("POSTGRES_USER", "autopatch")
		pgPass := getEnv("POSTGRES_PASSWORD", "autopatch")
		pgHost := getEnv("POSTGRES_HOST", "db")
		pgPort := getEnv("POSTGRES_PORT", "5432")
		pgDB := getEnv("POSTGRES_DB", "autopatch")
		dbURL = fmt.Sprintf(
			"postgres://%s:%s@%s:%s/%s?sslmode=disable",
			url.QueryEscape(pgUser),
			url.QueryEscape(pgPass),
			pgHost,
			pgPort,
			url.PathEscape(pgDB),
		)
	}

	vectorDim, err := getEnvInt("VECTOR_DIM", 768)
	if err != nil {
		return nil, fmt.Errorf("invalid VECTOR_DIM: %w", err)
	}
	if vectorDim <= 0 {
		return nil, fmt.Errorf("VECTOR_DIM must be positive, got %d", vectorDim)
	}

	simThreshold, err := getEnvFloat("SIMILARITY_THRESHOLD", 0.85)
	if err != nil {
		return nil, fmt.Errorf("invalid SIMILARITY_THRESHOLD: %w", err)
	}
	if simThreshold <= 0 || simThreshold > 1 {
		return nil, fmt.Errorf("SIMILARITY_THRESHOLD must be in (0, 1], got %v", simThreshold)
	}

	maxDiffLines, err := getEnvInt("MAX_DIFF_LINES", 200)
	if err != nil {
		return nil, fmt.Errorf("invalid MAX_DIFF_LINES: %w", err)
	}
	if maxDiffLines <= 0 {
		return nil, fmt.Errorf("MAX_DIFF_LINES must be positive, got %d", maxDiffLines)
	}

	workerCount, err := getEnvInt("WORKER_COUNT", 2)
	if err != nil {
		return nil, fmt.Errorf("invalid WORKER_COUNT: %w", err)
	}
	if workerCount <= 0 {
		return nil, fmt.Errorf("WORKER_COUNT must be positive, got %d", workerCount)
	}

	pollInterval, err := getEnvDuration("WORKER_POLL_INTERVAL", 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid WORKER_POLL_INTERVAL: %w", err)
	}
	if pollInterval <= 0 {
		return nil, fmt.Errorf("WORKER_POLL_INTERVAL must be positive, got %v", pollInterval)
	}

	sandboxTimeout, err := getEnvDuration("SANDBOX_TIMEOUT", 15*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("invalid SANDBOX_TIMEOUT: %w", err)
	}
	if sandboxTimeout <= 0 {
		return nil, fmt.Errorf("SANDBOX_TIMEOUT must be positive, got %v", sandboxTimeout)
	}

	cfg := &Config{
		Port:                port,
		DatabaseURL:         dbURL,
		TargetRepoURL:       strings.TrimSpace(os.Getenv("TARGET_REPO_URL")),
		GitHubToken:         strings.TrimSpace(os.Getenv("GITHUB_TOKEN")),
		GitHubWebhookSecret: strings.TrimSpace(os.Getenv("GITHUB_WEBHOOK_SECRET")),
		GitHubAPIURL:        strings.TrimRight(getEnv("GITHUB_API_URL", "https://api.github.com"), "/"),
		BaseBranch:          getEnv("BASE_BRANCH", "main"),
		LLMBaseURL:          NormalizeLLMBaseURL(getEnv("LLM_BASE_URL", "https://api.openai.com/v1")),
		LLMAPIKey:           strings.TrimSpace(os.Getenv("LLM_API_KEY")),
		EmbeddingModel:      getEnv("EMBEDDING_MODEL", "auto"),
		VectorDim:           vectorDim,
		TriageModel:         getEnv("TRIAGE_MODEL", "auto"),
		CodingModel:         getEnv("CODING_MODEL", "auto"),
		SimilarityThreshold: simThreshold,
		ProtectedPaths:      getEnv("PROTECTED_PATHS", ".env,docker-compose.yml,.github/workflows"),
		MaxDiffLines:        maxDiffLines,
		TestCommand:         getEnv("TEST_COMMAND", "npm test"),
		SandboxImage:        getEnv("SANDBOX_IMAGE", "autopatch-sandbox:latest"),
		CORSAllowedOrigin:   getEnv("CORS_ALLOWED_ORIGIN", "*"),
		AdminUser:           getEnv("ADMIN_USER", "admin"),
		AdminPassword:       strings.TrimSpace(os.Getenv("ADMIN_PASSWORD")),
		AlertWebhookURL:     strings.TrimSpace(os.Getenv("ALERT_WEBHOOK_URL")),
		PublicDashboardURL:  strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_DASHBOARD_URL")), "/"),
		WorkerCount:         workerCount,
		WorkerPollInterval:  pollInterval,
		SandboxTimeout:      sandboxTimeout,
	}

	return cfg, nil
}

// NormalizeLLMBaseURL trims trailing slashes and appends OpenAI-compatible path
// suffixes for known providers (Google Gemini, local Ollama/vLLM, OpenAI) when omitted.
func NormalizeLLMBaseURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "https://api.openai.com/v1"
	}

	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return trimmed
	}

	host := strings.ToLower(u.Hostname())
	port := u.Port()
	path := strings.TrimRight(u.Path, "/")

	switch {
	case host == "generativelanguage.googleapis.com":
		switch path {
		case "":
			u.Path = "/v1beta/openai"
		case "/v1beta", "/v1":
			u.Path = path + "/openai"
		default:
			u.Path = path
		}
	case port == "11434" || port == "8000" || strings.Contains(host, "ollama") || strings.Contains(host, "vllm"):
		if path == "" {
			u.Path = "/v1"
		} else {
			u.Path = path
		}
	case host == "api.openai.com":
		if path == "" {
			u.Path = "/v1"
		} else {
			u.Path = path
		}
	default:
		u.Path = path
	}

	return strings.TrimRight(u.String(), "/")
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	return strconv.Atoi(v)
}

func getEnvFloat(key string, fallback float64) (float64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	return strconv.ParseFloat(v, 64)
}

func getEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	return time.ParseDuration(v)
}
