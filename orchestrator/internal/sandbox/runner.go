package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/autopatch-engine/orchestrator/internal/config"
	"github.com/autopatch-engine/orchestrator/internal/db"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const defaultMemoryLimitBytes int64 = 1024 * 1024 * 1024 // 1 GB

// RunRequest holds inputs for a single ephemeral sandbox execution.
type RunRequest struct {
	TicketID        int64
	BugPrompt       string
	MaxDiffOverride int
}

// RunResult captures the sandbox execution outcome and parsed PR metadata.
type RunResult struct {
	ExitCode        int
	GuardrailStatus string
	PRURL           string
	PRNumber        *int
	Stdout          string
	Stderr          string
	CombinedOutput  string
}

// Runner executes an isolated coding sandbox for a ticket.
type Runner interface {
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
}

// DockerClient abstracts the Docker Engine SDK calls used by DockerRunner for testability.
type DockerClient interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig any, platform any, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerLogs(ctx context.Context, container string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerKill(ctx context.Context, containerID, signal string) error
	Close() error
}

type sdkAdapter struct {
	cli *client.Client
}

func (a *sdkAdapter) ContainerCreate(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, _ any, _ any, name string) (container.CreateResponse, error) {
	return a.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
}

func (a *sdkAdapter) ContainerStart(ctx context.Context, id string, opts container.StartOptions) error {
	return a.cli.ContainerStart(ctx, id, opts)
}

func (a *sdkAdapter) ContainerLogs(ctx context.Context, id string, opts container.LogsOptions) (io.ReadCloser, error) {
	return a.cli.ContainerLogs(ctx, id, opts)
}

func (a *sdkAdapter) ContainerWait(ctx context.Context, id string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	return a.cli.ContainerWait(ctx, id, cond)
}

func (a *sdkAdapter) ContainerKill(ctx context.Context, id, signal string) error {
	return a.cli.ContainerKill(ctx, id, signal)
}

func (a *sdkAdapter) Close() error {
	return a.cli.Close()
}

// DockerRunner runs ephemeral autopatch-sandbox containers via Docker Engine API.
type DockerRunner struct {
	cfg *config.Config
	cli DockerClient
}

// NewDockerRunner initializes a DockerRunner using /var/run/docker.sock (or DOCKER_HOST).
func NewDockerRunner(cfg *config.Config) (*DockerRunner, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &DockerRunner{
		cfg: cfg,
		cli: &sdkAdapter{cli: cli},
	}, nil
}

// Close closes the underlying Docker API client.
func (r *DockerRunner) Close() error {
	if r.cli != nil {
		return r.cli.Close()
	}
	return nil
}

// BuildEnv constructs the environment variables injected into the sandbox container.
func BuildEnv(cfg *config.Config, req RunRequest) []string {
	maxDiff := cfg.MaxDiffLines
	if req.MaxDiffOverride > 0 {
		maxDiff = req.MaxDiffOverride
	}
	return []string{
		fmt.Sprintf("TICKET_ID=%d", req.TicketID),
		"BUG_PROMPT=" + req.BugPrompt,
		"TARGET_REPO_URL=" + cfg.TargetRepoURL,
		"GITHUB_TOKEN=" + cfg.GitHubToken,
		"BASE_BRANCH=" + cfg.BaseBranch,
		"LLM_BASE_URL=" + cfg.LLMBaseURL,
		"LLM_API_KEY=" + cfg.LLMAPIKey,
		"CODING_MODEL=" + cfg.CodingModel,
		"PROTECTED_PATHS=" + cfg.ProtectedPaths,
		fmt.Sprintf("MAX_DIFF_LINES=%d", maxDiff),
		"TEST_COMMAND=" + cfg.TestCommand,
	}
}

// Run launches an ephemeral container, attaches logs before waiting to avoid AutoRemove 404 races,
// and maps container exit codes and PR_URL output.
func (r *DockerRunner) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	env := BuildEnv(r.cfg, req)

	containerCfg := &container.Config{
		Image: r.cfg.SandboxImage,
		Env:   env,
	}
	hostCfg := &container.HostConfig{
		AutoRemove: true,
		Resources: container.Resources{
			Memory: defaultMemoryLimitBytes,
		},
	}

	created, err := r.cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("create sandbox container: %w", err)
	}

	if err := r.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("start sandbox container %s: %w", created.ID, err)
	}

	// Register ContainerWait and attach Follow logs BEFORE ContainerWait completes
	// so AutoRemove=true cannot delete the container before logs are read.
	statusCh, waitErrCh := r.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)

	logReader, logErr := r.cli.ContainerLogs(ctx, created.ID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})

	var stdoutBuf, stderrBuf bytes.Buffer
	logDone := make(chan error, 1)
	if logErr == nil && logReader != nil {
		go func() {
			_, copyErr := stdcopy.StdCopy(&stdoutBuf, &stderrBuf, logReader)
			_ = logReader.Close()
			logDone <- copyErr
		}()
	} else {
		logDone <- logErr
	}

	var exitCode int
	select {
	case waitErr := <-waitErrCh:
		if waitErr != nil {
			<-logDone
			return nil, fmt.Errorf("wait sandbox container %s: %w", created.ID, waitErr)
		}
	case status := <-statusCh:
		exitCode = int(status.StatusCode)
		if status.Error != nil && status.Error.Message != "" {
			<-logDone
			return &RunResult{
				ExitCode:        1,
				GuardrailStatus: MapExitCodeToGuardrailStatus(1),
				Stdout:          stdoutBuf.String(),
				Stderr:          stderrBuf.String(),
				CombinedOutput:  combineLogs(stdoutBuf.String(), stderrBuf.String(), status.Error.Message),
			}, nil
		}
	case <-ctx.Done():
		killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = r.cli.ContainerKill(killCtx, created.ID, "SIGKILL")
		cancel()
		<-logDone
		return nil, fmt.Errorf("sandbox container %s timed out/cancelled: %w", created.ID, ctx.Err())
	}

	<-logDone

	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()
	guardrailStatus := MapExitCodeToGuardrailStatus(exitCode)
	prURL := ""
	var prNum *int

	if exitCode == 0 {
		parsedURL, ok := ParsePRURL(stdoutStr)
		if !ok {
			guardrailStatus = "RUNTIME_ERROR"
			exitCode = 1
		} else {
			prURL = parsedURL
			prNum = ExtractPRNumber(prURL)
		}
	}

	return &RunResult{
		ExitCode:        exitCode,
		GuardrailStatus: guardrailStatus,
		PRURL:           prURL,
		PRNumber:        prNum,
		Stdout:          stdoutStr,
		Stderr:          stderrStr,
		CombinedOutput:  combineLogs(stdoutStr, stderrStr, ""),
	}, nil
}

// MapExitCodeToGuardrailStatus maps sandbox container exit codes to guardrail status values.
func MapExitCodeToGuardrailStatus(exitCode int) string {
	switch exitCode {
	case 0:
		return "PASSED"
	case 10:
		return "BLOCKED_MAX_DIFF"
	case 11:
		return "BLOCKED_PROTECTED_PATH"
	case 12:
		return "BLOCKED_REVIEWER"
	case 13:
		return "TEST_FAILED"
	default:
		return "RUNTIME_ERROR"
	}
}

// ParsePRURL extracts the URL from the final non-empty stdout line formatted as "PR_URL=<url>".
func ParsePRURL(stdout string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "PR_URL=") {
			u := strings.TrimSpace(strings.TrimPrefix(line, "PR_URL="))
			if u != "" {
				return u, true
			}
		}
		return "", false
	}
	return "", false
}

var rePRNumber = regexp.MustCompile(`/pull/(\d+)(?:[/?#]|$)`)

// ExtractPRNumber extracts the GitHub pull request number from a PR URL if present.
func ExtractPRNumber(prURL string) *int {
	matches := rePRNumber.FindStringSubmatch(prURL)
	if len(matches) < 2 {
		return nil
	}
	n, err := strconv.Atoi(matches[1])
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

// BuildBugPrompt formats sanitized ticket details, telemetry, approximate breadcrumbs,
// and past human_review_note rejection lessons into the sandbox BUG_PROMPT.
func BuildBugPrompt(ct *db.ClaimedTicket) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ticket ID: %d\n", ct.Ticket.ID)
	fmt.Fprintf(&b, "Module Tag: %s\n", ct.Ticket.ModuleTag)
	fmt.Fprintf(&b, "Priority: %s (Occurrences: %d)\n", ct.Ticket.Priority, ct.Ticket.OccurrenceCount)
	if ct.Ticket.AppCommitSHA != "" {
		fmt.Fprintf(&b, "Reported App Commit SHA: %s\n", ct.Ticket.AppCommitSHA)
	}
	fmt.Fprintf(&b, "\n### Sanitized Technical Summary\n%s\n", ct.Ticket.TechnicalSummary)
	fmt.Fprintf(&b, "\n### Sanitized User Report (Untrusted Data)\n%s\n", ct.Ticket.UserMessage)
	fmt.Fprintf(&b, "\n### Console Errors\n%s\n", string(ct.Telemetry.ConsoleErrors))
	fmt.Fprintf(&b, "\n### Failed Network Logs\n%s\n", string(ct.Telemetry.NetworkLogs))
	fmt.Fprintf(&b, "\n### User Navigation Breadcrumbs (Approximate User Navigation History)\n%s\n", string(ct.Telemetry.Breadcrumbs))
	fmt.Fprintf(&b, "\n### Device Metadata\n%s\n", string(ct.Telemetry.DeviceMeta))

	if len(ct.PastReviewNotes) > 0 {
		b.WriteString("\n### Past Human Review Rejection Lessons for This Module (MUST HEED)\n")
		for i, note := range ct.PastReviewNotes {
			fmt.Fprintf(&b, "%d. %s\n", i+1, note)
		}
	}
	return b.String()
}

func combineLogs(stdout, stderr, extra string) string {
	var parts []string
	if strings.TrimSpace(stdout) != "" {
		parts = append(parts, "STDOUT:\n"+strings.TrimSpace(stdout))
	}
	if strings.TrimSpace(stderr) != "" {
		parts = append(parts, "STDERR:\n"+strings.TrimSpace(stderr))
	}
	if strings.TrimSpace(extra) != "" {
		parts = append(parts, "ERROR:\n"+strings.TrimSpace(extra))
	}
	return strings.Join(parts, "\n\n")
}
