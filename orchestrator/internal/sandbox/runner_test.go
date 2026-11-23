package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/autopatch-engine/orchestrator/internal/config"
	"github.com/autopatch-engine/orchestrator/internal/db"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

type fakeDockerClient struct {
	createdCfg     *container.Config
	createdHostCfg *container.HostConfig
	stdout         string
	stderr         string
	exitCode       int64
	callOrder      []string
}

func (f *fakeDockerClient) ContainerCreate(_ context.Context, cfg *container.Config, hostCfg *container.HostConfig, _ any, _ any, _ string) (container.CreateResponse, error) {
	f.callOrder = append(f.callOrder, "create")
	f.createdCfg = cfg
	f.createdHostCfg = hostCfg
	return container.CreateResponse{ID: "sandbox-123"}, nil
}

func (f *fakeDockerClient) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	f.callOrder = append(f.callOrder, "start")
	return nil
}

func (f *fakeDockerClient) ContainerLogs(_ context.Context, _ string, opts container.LogsOptions) (io.ReadCloser, error) {
	f.callOrder = append(f.callOrder, "logs")
	if !opts.Follow || !opts.ShowStdout || !opts.ShowStderr {
		panic("expected Follow, ShowStdout, and ShowStderr to be true")
	}
	var muxed bytes.Buffer
	if f.stdout != "" {
		w := stdcopy.NewStdWriter(&muxed, stdcopy.Stdout)
		_, _ = io.WriteString(w, f.stdout)
	}
	if f.stderr != "" {
		w := stdcopy.NewStdWriter(&muxed, stdcopy.Stderr)
		_, _ = io.WriteString(w, f.stderr)
	}
	return io.NopCloser(&muxed), nil
}

func (f *fakeDockerClient) ContainerWait(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	f.callOrder = append(f.callOrder, "wait")
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error)
	statusCh <- container.WaitResponse{StatusCode: f.exitCode}
	return statusCh, errCh
}

func (f *fakeDockerClient) ContainerKill(_ context.Context, _, _ string) error {
	return nil
}

func (f *fakeDockerClient) Close() error {
	return nil
}

func TestMapExitCodeToGuardrailStatus(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, "PASSED"},
		{10, "BLOCKED_MAX_DIFF"},
		{11, "BLOCKED_PROTECTED_PATH"},
		{12, "BLOCKED_REVIEWER"},
		{13, "TEST_FAILED"},
		{1, "RUNTIME_ERROR"},
		{99, "RUNTIME_ERROR"},
	}
	for _, tc := range cases {
		if got := MapExitCodeToGuardrailStatus(tc.code); got != tc.want {
			t.Fatalf("exit code %d: expected %q, got %q", tc.code, tc.want, got)
		}
	}
}

func TestParsePRURLAndExtractPRNumber(t *testing.T) {
	stdout := "Cloning repo...\nRunning tests...\nPR_URL=https://github.com/acme/webapp/pull/128\n\n"
	url, ok := ParsePRURL(stdout)
	if !ok || url != "https://github.com/acme/webapp/pull/128" {
		t.Fatalf("unexpected ParsePRURL result: (%q, %v)", url, ok)
	}
	num := ExtractPRNumber(url)
	if num == nil || *num != 128 {
		t.Fatalf("expected PR number 128, got %v", num)
	}

	if _, ok := ParsePRURL("Done without PR_URL line\n"); ok {
		t.Fatalf("expected ParsePRURL to fail when last non-empty line is not PR_URL=")
	}
}

func TestDockerRunnerSuccessAndContract(t *testing.T) {
	fake := &fakeDockerClient{
		stdout:   "Running patch...\nPR_URL=https://github.com/org/repo/pull/77\n",
		stderr:   "warn: something\n",
		exitCode: 0,
	}
	cfg := &config.Config{
		SandboxImage:   "autopatch-sandbox:latest",
		TargetRepoURL:  "https://github.com/org/repo.git",
		GitHubToken:    "ghp_secret",
		BaseBranch:     "main",
		LLMBaseURL:     "https://api.openai.com/v1",
		LLMAPIKey:      "sk-secret",
		CodingModel:    "gpt-4o",
		ProtectedPaths: ".env,docker-compose.yml",
		MaxDiffLines:   200,
		TestCommand:    "npm test",
	}
	runner := &DockerRunner{cfg: cfg, cli: fake}

	res, err := runner.Run(context.Background(), RunRequest{
		TicketID:  42,
		BugPrompt: "Fix checkout NPE",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.GuardrailStatus != "PASSED" {
		t.Fatalf("expected PASSED, got %q", res.GuardrailStatus)
	}
	if res.PRURL != "https://github.com/org/repo/pull/77" {
		t.Fatalf("unexpected PRURL %q", res.PRURL)
	}
	if res.PRNumber == nil || *res.PRNumber != 77 {
		t.Fatalf("expected PRNumber 77, got %v", res.PRNumber)
	}
	if !fake.createdHostCfg.AutoRemove {
		t.Fatalf("expected AutoRemove=true")
	}
	if fake.createdHostCfg.Resources.Memory != 1024*1024*1024 {
		t.Fatalf("expected 1GB memory limit, got %d", fake.createdHostCfg.Resources.Memory)
	}
	envJoined := strings.Join(fake.createdCfg.Env, "\n")
	for _, k := range []string{
		"TICKET_ID=42",
		"BUG_PROMPT=Fix checkout NPE",
		"TARGET_REPO_URL=",
		"GITHUB_TOKEN=",
		"BASE_BRANCH=main",
		"LLM_BASE_URL=",
		"LLM_API_KEY=",
		"CODING_MODEL=gpt-4o",
		"PROTECTED_PATHS=.env,docker-compose.yml",
		"MAX_DIFF_LINES=200",
		"TEST_COMMAND=npm test",
	} {
		if !strings.Contains(envJoined, k) {
			t.Fatalf("missing contract env %q in container env", k)
		}
	}
}

func TestDockerRunnerExitCodes(t *testing.T) {
	cfg := &config.Config{SandboxImage: "autopatch-sandbox:latest", MaxDiffLines: 200}
	for code, want := range map[int64]string{
		10: "BLOCKED_MAX_DIFF",
		11: "BLOCKED_PROTECTED_PATH",
		12: "BLOCKED_REVIEWER",
		13: "TEST_FAILED",
		1:  "RUNTIME_ERROR",
	} {
		fake := &fakeDockerClient{stderr: "gate triggered", exitCode: code}
		runner := &DockerRunner{cfg: cfg, cli: fake}
		res, err := runner.Run(context.Background(), RunRequest{TicketID: 1, BugPrompt: "test"})
		if err != nil {
			t.Fatalf("Run(%d) unexpected err: %v", code, err)
		}
		if res.GuardrailStatus != want {
			t.Fatalf("code %d: expected %q, got %q", code, want, res.GuardrailStatus)
		}
	}
}

func TestBuildBugPromptIncludesBreadcrumbsAndPastNotes(t *testing.T) {
	prompt := BuildBugPrompt(&db.ClaimedTicket{
		Ticket: db.Ticket{
			ID:               9,
			ModuleTag:        "checkout",
			Priority:         "HIGH",
			OccurrenceCount:  3,
			AppCommitSHA:     "abc123",
			TechnicalSummary: "Null total in cart",
			UserMessage:      "Cart total shows NaN",
		},
		Telemetry: db.Telemetry{
			Breadcrumbs:   json.RawMessage(`[{"type":"click","text":"Checkout"}]`),
			ConsoleErrors: json.RawMessage(`["TypeError"]`),
			NetworkLogs:   json.RawMessage(`[{"status":500}]`),
			DeviceMeta:    json.RawMessage(`{"ua":"Chrome"}`),
		},
		PastReviewNotes: []string{"Keep tax calculation pure"},
	})
	if !strings.Contains(prompt, "Approximate User Navigation History") {
		t.Fatalf("expected breadcrumbs label in prompt")
	}
	if !strings.Contains(prompt, "Keep tax calculation pure") {
		t.Fatalf("expected past review note in prompt")
	}
}

func TestBuildEnvMaxDiffOverride(t *testing.T) {
	cfg := &config.Config{
		CodingModel:  "qwen2.5-coder:7b",
		MaxDiffLines: 200,
	}

	envDefault := strings.Join(BuildEnv(cfg, RunRequest{TicketID: 1, BugPrompt: "bug"}), "\n")
	if !strings.Contains(envDefault, "MAX_DIFF_LINES=200") {
		t.Fatalf("expected default MAX_DIFF_LINES=200, got %s", envDefault)
	}
	if !strings.Contains(envDefault, "CODING_MODEL=qwen2.5-coder:7b") {
		t.Fatalf("expected resolved CODING_MODEL in env, got %s", envDefault)
	}

	envOverride := strings.Join(BuildEnv(cfg, RunRequest{TicketID: 1, BugPrompt: "bug", MaxDiffOverride: 500}), "\n")
	if !strings.Contains(envOverride, "MAX_DIFF_LINES=500") {
		t.Fatalf("expected overridden MAX_DIFF_LINES=500, got %s", envOverride)
	}
}
