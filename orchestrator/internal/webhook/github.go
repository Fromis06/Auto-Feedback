package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/autopatch-engine/orchestrator/internal/config"
	"github.com/autopatch-engine/orchestrator/internal/llm"
)

// PRHead describes the source branch of an open pull request.
type PRHead struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// OpenPR represents an open GitHub pull request targeting BASE_BRANCH.
type OpenPR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Head   PRHead `json:"head"`
}

// RebaseRunner executes git rebase + LLM conflict resolution + TEST_COMMAND + force push.
type RebaseRunner interface {
	RebaseAndVerifyPR(ctx context.Context, pr OpenPR, ticketContext string) error
}

// GitHubClient calls the GitHub REST API.
type GitHubClient struct {
	httpClient *http.Client
	apiURL     string
	token      string
}

// NewGitHubClient constructs a GitHub REST API client.
func NewGitHubClient(apiURL, token string) *GitHubClient {
	return &GitHubClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		apiURL:     strings.TrimRight(apiURL, "/"),
		token:      token,
	}
}

// ParseGitHubOwnerRepo extracts owner and repo name from an HTTPS or SSH GitHub URL.
func ParseGitHubOwnerRepo(repoURL string) (string, string, bool) {
	trimmed := strings.TrimSpace(repoURL)
	if trimmed == "" {
		return "", "", false
	}

	if strings.HasPrefix(trimmed, "git@") {
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			return "", "", false
		}
		pathPart := strings.TrimSuffix(strings.Trim(parts[1], "/"), ".git")
		segs := strings.Split(pathPart, "/")
		if len(segs) < 2 {
			return "", "", false
		}
		return segs[len(segs)-2], segs[len(segs)-1], true
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", "", false
	}
	if u.Scheme == "file" {
		return "", "", false
	}
	pathPart := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	segs := strings.Split(pathPart, "/")
	if len(segs) < 2 {
		return "", "", false
	}
	owner := segs[len(segs)-2]
	repo := segs[len(segs)-1]
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

// VerifyGitHubSignature validates GitHub's X-Hub-Signature-256 HMAC-SHA256 header.
func VerifyGitHubSignature(secret string, payload []byte, signatureHeader string) bool {
	if secret == "" {
		return false
	}
	if !strings.HasPrefix(signatureHeader, "sha256=") {
		return false
	}
	sigHex := strings.TrimPrefix(signatureHeader, "sha256=")
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	expected := mac.Sum(nil)
	return hmac.Equal(sigBytes, expected)
}

var reTicketBranch = regexp.MustCompile(`^ai-fix/ticket-(\d+)$`)

// ExtractTicketIDFromBranch extracts the numeric ticket ID from branches like "ai-fix/ticket-42".
func ExtractTicketIDFromBranch(branch string) int64 {
	m := reTicketBranch.FindStringSubmatch(strings.TrimSpace(branch))
	if len(m) < 2 {
		return 0
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// CheckOutdatedModule checks whether appCommitSHA is behind BASE_BRANCH HEAD and whether
// files matching moduleTag were already modified since appCommitSHA.
func (g *GitHubClient) CheckOutdatedModule(ctx context.Context, owner, repo, baseBranch, appCommitSHA, moduleTag string) (bool, string, error) {
	commitURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s", g.apiURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(baseBranch))
	var headResp struct {
		SHA string `json:"sha"`
	}
	if err := g.getJSON(ctx, commitURL, &headResp); err != nil {
		return false, "", err
	}
	if headResp.SHA == "" || strings.EqualFold(headResp.SHA, appCommitSHA) {
		return false, headResp.SHA, nil
	}

	compareURL := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s",
		g.apiURL,
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.PathEscape(appCommitSHA),
		url.PathEscape(headResp.SHA),
	)
	var cmpResp struct {
		Status   string `json:"status"`
		BehindBy int    `json:"behind_by"`
		Files    []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := g.getJSON(ctx, compareURL, &cmpResp); err != nil {
		return false, headResp.SHA, err
	}

	// Only mark outdated if BASE_BRANCH actually moved ahead of appCommitSHA
	// and a file matching the affected module_tag was modified.
	if cmpResp.Status != "ahead" || len(cmpResp.Files) == 0 {
		return false, headResp.SHA, nil
	}
	modLower := strings.ToLower(strings.TrimSpace(moduleTag))
	if modLower == "" || modLower == "general" {
		return false, headResp.SHA, nil
	}
	for _, f := range cmpResp.Files {
		if strings.Contains(strings.ToLower(f.Filename), modLower) {
			return true, headResp.SHA, nil
		}
	}
	return false, headResp.SHA, nil
}

// ListOpenAIFixPRs fetches open PRs targeting baseBranch whose head ref starts with "ai-fix/".
func (g *GitHubClient) ListOpenAIFixPRs(ctx context.Context, owner, repo, baseBranch string) ([]OpenPR, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&base=%s&per_page=100",
		g.apiURL,
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.QueryEscape(baseBranch),
	)
	var prs []OpenPR
	if err := g.getJSON(ctx, u, &prs); err != nil {
		return nil, err
	}
	var out []OpenPR
	for _, pr := range prs {
		if strings.HasPrefix(pr.Head.Ref, "ai-fix/") {
			out = append(out, pr)
		}
	}
	return out, nil
}

// GetPRReviewComments aggregates review comments, review bodies, and issue comments for a PR.
func (g *GitHubClient) GetPRReviewComments(ctx context.Context, owner, repo string, prNumber int) (string, error) {
	var notes []string

	reviewsURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews", g.apiURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)
	var reviews []struct {
		Body  string `json:"body"`
		State string `json:"state"`
	}
	if err := g.getJSON(ctx, reviewsURL, &reviews); err == nil {
		for _, rv := range reviews {
			if body := strings.TrimSpace(rv.Body); body != "" {
				notes = append(notes, fmt.Sprintf("[Review %s] %s", rv.State, body))
			}
		}
	}

	prCommentsURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/comments", g.apiURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)
	var prComments []struct {
		Path string `json:"path"`
		Body string `json:"body"`
	}
	if err := g.getJSON(ctx, prCommentsURL, &prComments); err == nil {
		for _, c := range prComments {
			if body := strings.TrimSpace(c.Body); body != "" {
				if c.Path != "" {
					notes = append(notes, fmt.Sprintf("[%s] %s", c.Path, body))
				} else {
					notes = append(notes, body)
				}
			}
		}
	}

	issueCommentsURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", g.apiURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)
	var issueComments []struct {
		Body string `json:"body"`
	}
	if err := g.getJSON(ctx, issueCommentsURL, &issueComments); err == nil {
		for _, c := range issueComments {
			if body := strings.TrimSpace(c.Body); body != "" {
				notes = append(notes, body)
			}
		}
	}

	return strings.Join(notes, "\n"), nil
}

func (g *GitHubClient) getJSON(ctx context.Context, targetURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return fmt.Errorf("create github request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return fmt.Errorf("read github response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github API %s returned status %d: %s", targetURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode github JSON: %w", err)
	}
	return nil
}

// GitRebaseRunner performs real git rebase operations, resolves conflicts via CODING_MODEL,
// re-runs TEST_COMMAND, and force-pushes the rebased PR branch.
type GitRebaseRunner struct {
	cfg    *config.Config
	llmSvc llm.Service
}

// NewGitRebaseRunner creates a GitRebaseRunner.
func NewGitRebaseRunner(cfg *config.Config, llmSvc llm.Service) *GitRebaseRunner {
	return &GitRebaseRunner{cfg: cfg, llmSvc: llmSvc}
}

// RebaseAndVerifyPR clones TARGET_REPO_URL, rebases pr.Head.Ref onto BASE_BRANCH,
// invokes llmSvc.ResolveConflict on any merge conflict, executes TEST_COMMAND,
// and force-pushes origin/<pr.Head.Ref>.
func (r *GitRebaseRunner) RebaseAndVerifyPR(ctx context.Context, pr OpenPR, ticketContext string) error {
	if r.cfg.TargetRepoURL == "" {
		return fmt.Errorf("TARGET_REPO_URL is not configured")
	}
	tmpDir, err := os.MkdirTemp("", "autopatch-rebase-*")
	if err != nil {
		return fmt.Errorf("create temp rebase dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cloneURL := injectGitHubToken(r.cfg.TargetRepoURL, r.cfg.GitHubToken)

	if _, err := runCmd(ctx, "", "git", "clone", cloneURL, tmpDir); err != nil {
		return fmt.Errorf("git clone failed: %s", redactToken(err.Error(), r.cfg.GitHubToken))
	}
	if _, err := runCmd(ctx, tmpDir, "git", "config", "user.name", "autopatch-bot"); err != nil {
		return fmt.Errorf("git config user.name: %w", err)
	}
	if _, err := runCmd(ctx, tmpDir, "git", "config", "user.email", "autopatch-bot@users.noreply.github.com"); err != nil {
		return fmt.Errorf("git config user.email: %w", err)
	}
	if _, err := runCmd(ctx, tmpDir, "git", "fetch", "origin", r.cfg.BaseBranch, pr.Head.Ref); err != nil {
		return fmt.Errorf("git fetch failed: %s", redactToken(err.Error(), r.cfg.GitHubToken))
	}
	if _, err := runCmd(ctx, tmpDir, "git", "checkout", "-B", pr.Head.Ref, "origin/"+pr.Head.Ref); err != nil {
		return fmt.Errorf("git checkout %s: %w", pr.Head.Ref, err)
	}

	_, rebaseErr := runCmd(ctx, tmpDir, "git", "rebase", "origin/"+r.cfg.BaseBranch)
	if rebaseErr != nil {
		if err := r.resolveRebaseConflicts(ctx, tmpDir, ticketContext); err != nil {
			_, _ = runCmd(ctx, tmpDir, "git", "rebase", "--abort")
			return fmt.Errorf("rebase conflict resolution failed for %s: %w", pr.Head.Ref, err)
		}
	}

	if strings.TrimSpace(r.cfg.TestCommand) != "" {
		if out, err := runShellCmd(ctx, tmpDir, r.cfg.TestCommand); err != nil {
			return fmt.Errorf("regression test command %q failed after rebase: %w\n%s", r.cfg.TestCommand, err, out)
		}
	}

	if _, err := runCmd(ctx, tmpDir, "git", "push", "--force-with-lease", "origin", pr.Head.Ref); err != nil {
		return fmt.Errorf("git force-push %s failed: %s", pr.Head.Ref, redactToken(err.Error(), r.cfg.GitHubToken))
	}
	return nil
}

func (r *GitRebaseRunner) resolveRebaseConflicts(ctx context.Context, repoDir, ticketContext string) error {
	const maxRebaseSteps = 10
	for step := 0; step < maxRebaseSteps; step++ {
		conflictedOut, err := runCmd(ctx, repoDir, "git", "diff", "--name-only", "--diff-filter=U")
		if err != nil {
			return fmt.Errorf("list conflicted files: %w", err)
		}
		var files []string
		for _, line := range strings.Split(strings.ReplaceAll(conflictedOut, "\r\n", "\n"), "\n") {
			if f := strings.TrimSpace(line); f != "" {
				files = append(files, f)
			}
		}
		if len(files) == 0 {
			return fmt.Errorf("rebase failed without unmerged conflict files")
		}

		for _, relPath := range files {
			absPath := filepath.Join(repoDir, filepath.FromSlash(relPath))
			conflictedBytes, readErr := os.ReadFile(absPath)
			conflictedCode := string(conflictedBytes)
			if readErr != nil {
				if !os.IsNotExist(readErr) {
					return fmt.Errorf("read conflicted file %s: %w", relPath, readErr)
				}
				conflictedCode = "(file does not exist in working tree; delete/modify conflict)"
			}

			baseCode, _ := runCmd(ctx, repoDir, "git", "show", "origin/"+r.cfg.BaseBranch+":"+relPath)
			prCode, prErr := runCmd(ctx, repoDir, "git", "show", "REBASE_HEAD:"+relPath)
			if prErr != nil {
				prCode, _ = runCmd(ctx, repoDir, "git", "show", ":3:"+relPath)
			}

			resolved, llmErr := r.llmSvc.ResolveConflict(ctx, llm.ConflictResolutionRequest{
				FilePath:       relPath,
				BaseBranchCode: baseCode,
				PRBranchCode:   prCode,
				ConflictedCode: conflictedCode,
				TicketContext:  ticketContext,
			})
			if llmErr != nil {
				return fmt.Errorf("LLM ResolveConflict on %s: %w", relPath, llmErr)
			}

			if mkErr := os.MkdirAll(filepath.Dir(absPath), 0o755); mkErr != nil {
				return fmt.Errorf("create parent dir for %s: %w", relPath, mkErr)
			}
			if writeErr := os.WriteFile(absPath, []byte(resolved), 0o644); writeErr != nil {
				return fmt.Errorf("write resolved file %s: %w", relPath, writeErr)
			}
			if _, addErr := runCmd(ctx, repoDir, "git", "add", relPath); addErr != nil {
				return fmt.Errorf("git add %s: %w", relPath, addErr)
			}
		}

		_, contErr := runCmdWithEnv(ctx, repoDir, []string{"GIT_EDITOR=true"}, "git", "-c", "core.editor=true", "rebase", "--continue")
		if contErr == nil {
			return nil
		}
	}
	return fmt.Errorf("exceeded max rebase conflict resolution steps (%d)", maxRebaseSteps)
}

func injectGitHubToken(repoURL, token string) string {
	if token == "" {
		return repoURL
	}
	u, err := url.Parse(repoURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return repoURL
	}
	u.User = url.UserPassword("x-access-token", token)
	return u.String()
}

func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[REDACTED]")
}

func runCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	return runCmdWithEnv(ctx, dir, nil, name, args...)
}

func runCmdWithEnv(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err != nil {
		return buf.String(), fmt.Errorf("%s %v failed: %w: %s", name, args, err, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}

func runShellCmd(ctx context.Context, dir, command string) (string, error) {
	if runtime.GOOS == "windows" {
		return runCmd(ctx, dir, "cmd", "/C", command)
	}
	return runCmd(ctx, dir, "sh", "-c", command)
}
