#!/usr/bin/env bash
set -uo pipefail

log() {
    printf '[autopatch-sandbox] %s\n' "$*" >&2
}

TICKET_ID="${TICKET_ID:-}"
BUG_PROMPT="${BUG_PROMPT:-}"
TARGET_REPO_URL="${TARGET_REPO_URL:-}"
GITHUB_TOKEN="${GITHUB_TOKEN:-}"
BASE_BRANCH="${BASE_BRANCH:-main}"
LLM_BASE_URL="${LLM_BASE_URL:-https://api.openai.com/v1}"
LLM_API_KEY="${LLM_API_KEY:-}"
CODING_MODEL="${CODING_MODEL:-gpt-4o}"
PROTECTED_PATHS="${PROTECTED_PATHS:-}"
MAX_DIFF_LINES="${MAX_DIFF_LINES:-200}"
TEST_COMMAND="${TEST_COMMAND:-npm test}"
MAX_RETRIES="${MAX_RETRIES:-3}"

if [ -z "$TICKET_ID" ] || [ -z "$TARGET_REPO_URL" ]; then
    log "ERROR: TICKET_ID and TARGET_REPO_URL are required"
    exit 1
fi

if ! [[ "$MAX_DIFF_LINES" =~ ^[0-9]+$ ]]; then
    log "ERROR: MAX_DIFF_LINES must be a non-negative integer (got '$MAX_DIFF_LINES')"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -n "${AUTOPATCH_AGENT_PATH:-}" ]; then
    AGENT_BIN="$AUTOPATCH_AGENT_PATH"
elif [ -f "/usr/local/bin/autopatch-agent.py" ]; then
    AGENT_BIN="/usr/local/bin/autopatch-agent.py"
else
    AGENT_BIN="${SCRIPT_DIR}/agent.py"
fi

PYTHON_BIN="$(command -v python3 || command -v python || true)"
if [ -z "$PYTHON_BIN" ] || [ ! -f "$AGENT_BIN" ]; then
    log "ERROR: Python interpreter or agent.py not found ($PYTHON_BIN / $AGENT_BIN)"
    exit 1
fi

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/autopatch-sandbox.XXXXXX")"
cleanup() {
    rm -rf "$WORK_DIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

REPO_DIR="${WORK_DIR}/repo"
PROMPT_FILE="${WORK_DIR}/prompt.txt"
DIFF_FILE="${WORK_DIR}/current.diff"
REVIEW_ERR_FILE="${WORK_DIR}/review.err"
TEST_OUT_FILE="${WORK_DIR}/test.out"
ROOT_CAUSE_FILE="${WORK_DIR}/root_cause.txt"
PR_BODY_FILE="${WORK_DIR}/pr_body.md"
PRE_IGNORED_FILE="${WORK_DIR}/pre_ignored_protected.txt"

export GIT_TERMINAL_PROMPT=0
if [ -n "$GITHUB_TOKEN" ]; then
    export GH_TOKEN="$GITHUB_TOKEN"
fi

CLONE_URL="$TARGET_REPO_URL"
if [ -n "$GITHUB_TOKEN" ] && [[ "$TARGET_REPO_URL" == https://* ]] && [[ "$TARGET_REPO_URL" != *"@"* ]]; then
    CLONE_URL="${TARGET_REPO_URL/https:\/\//https:\/\/x-access-token:${GITHUB_TOKEN}@}"
fi

log "Cloning target repository into ${REPO_DIR} (base branch: ${BASE_BRANCH})"
if ! git clone --depth 1 --branch "$BASE_BRANCH" "$CLONE_URL" "$REPO_DIR" >&2 2>&1; then
    log "Branch clone failed; retrying shallow clone of default branch"
    rm -rf "$REPO_DIR"
    if ! git clone --depth 1 "$CLONE_URL" "$REPO_DIR" >&2 2>&1; then
        log "ERROR: Failed to clone repository"
        exit 1
    fi
fi

cd "$REPO_DIR" || exit 1

git config user.name "autopatch-bot" >&2 || exit 1
git config user.email "autopatch@localhost" >&2 || exit 1

BRANCH_NAME="ai-fix/ticket-${TICKET_ID}"
log "Checking out branch ${BRANCH_NAME}"
if ! git checkout -B "$BRANCH_NAME" >&2 2>&1; then
    log "ERROR: Failed to create branch ${BRANCH_NAME}"
    exit 1
fi

build_prompt() {
    cat > "$PROMPT_FILE" <<EOF
Ticket ID: ${TICKET_ID}
Base Branch: ${BASE_BRANCH}
Protected Paths (DO NOT MODIFY): ${PROTECTED_PATHS:-none}
Max Diff Lines Allowed: ${MAX_DIFF_LINES}
Regression Test Command: ${TEST_COMMAND}

=== SANITIZED BUG CONTEXT, LOGS, APPROXIMATE USER BREADCRUMBS & PAST HUMAN REVIEW NOTES ===
Note: User navigation breadcrumbs below are approximate client-side history and may be incomplete.
${BUG_PROMPT}
EOF
}

match_protected_path() {
    local file="$1"
    [ -z "$PROTECTED_PATHS" ] && return 1
    local IFS=','
    local protected_arr
    read -ra protected_arr <<< "$PROTECTED_PATHS"
    local raw_entry entry
    for raw_entry in "${protected_arr[@]}"; do
        entry="$(printf '%s' "$raw_entry" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e 's|^\./||' -e 's|/*$||')"
        [ -z "$entry" ] && continue
        case "$file" in
            $entry|$entry/*)
                printf '%s' "$entry"
                return 0
                ;;
        esac
    done
    return 1
}

ignored_path_signature() {
    local path="$1"
    if [ -L "$path" ]; then
        printf 'symlink:%s' "$(readlink "$path" 2>/dev/null || printf '?')"
    elif [ -f "$path" ]; then
        printf 'file:%s' "$(git hash-object -- "$path" 2>/dev/null || printf 'err')"
    elif [ -d "$path" ]; then
        printf 'dir'
    else
        printf 'other'
    fi
}

snapshot_ignored_protected() {
    local out_file="$1"
    : > "$out_file"
    [ -z "$PROTECTED_PATHS" ] && return 0
    local path sig
    while IFS= read -r -d '' path; do
        [ -z "$path" ] && continue
        if match_protected_path "$path" >/dev/null; then
            sig="$(ignored_path_signature "$path")"
            printf '%s\t%s\n' "$sig" "$path" >> "$out_file"
        fi
    done < <(git -c core.quotePath=false ls-files --others --ignored --exclude-standard -z)
}

check_protected_paths() {
    [ -z "$PROTECTED_PATHS" ] && return 0
    local file matched_rule sig record

    while IFS= read -r -d '' file; do
        [ -z "$file" ] && continue
        if matched_rule="$(match_protected_path "$file")"; then
            log "BLAST RADIUS VIOLATION: Protected path touched: ${file} (rule: ${matched_rule})"
            return 1
        fi
    done < <(git -c core.quotePath=false diff HEAD --name-only -z)

    while IFS= read -r -d '' file; do
        [ -z "$file" ] && continue
        if matched_rule="$(match_protected_path "$file")"; then
            sig="$(ignored_path_signature "$file")"
            record="$(printf '%s\t%s' "$sig" "$file")"
            if ! TARGET_RECORD="$record" awk 'BEGIN{t=ENVIRON["TARGET_RECORD"]; found=0} $0==t{found=1;exit} END{exit !found}' "$PRE_IGNORED_FILE"; then
                log "BLAST RADIUS VIOLATION: Protected gitignored path touched: ${file} (rule: ${matched_rule})"
                return 1
            fi
        fi
    done < <(git -c core.quotePath=false ls-files --others --ignored --exclude-standard -z)

    return 0
}

compute_diff_lines() {
    git -c core.quotePath=false diff HEAD --numstat | awk '{
        if ($1 ~ /^[0-9]+$/) added += $1;
        if ($2 ~ /^[0-9]+$/) deleted += $2;
    } END { print added + deleted + 0 }'
}

RETRY_FEEDBACK=""
LAST_TEST_OUTPUT=""
LAST_FAILURE_CODE=1

for attempt in $(seq 1 "$MAX_RETRIES"); do
    log "=== Self-healing attempt ${attempt}/${MAX_RETRIES} ==="
    if [ "$attempt" -gt 1 ]; then
        git reset --hard HEAD >&2 2>&1 || exit 1
        git clean -fd >&2 2>&1 || exit 1
    fi
    snapshot_ignored_protected "$PRE_IGNORED_FILE"
    build_prompt

    if ! "$PYTHON_BIN" "$AGENT_BIN" \
        --mode code \
        --repo-dir "$REPO_DIR" \
        --prompt-file "$PROMPT_FILE" \
        --feedback "$RETRY_FEEDBACK" \
        --summary-out "$ROOT_CAUSE_FILE" \
        --base-url "$LLM_BASE_URL" \
        --api-key "$LLM_API_KEY" \
        --model "$CODING_MODEL" >&2; then
        log "ERROR: Coding agent failed on attempt ${attempt}"
        exit 1
    fi

    # Track newly created files in git diff so blast-radius gates inspect them.
    if ! git add -N . >&2 2>&1; then
        log "ERROR: git add -N . failed"
        exit 1
    fi

    if ! check_protected_paths; then
        exit 11
    fi

    DIFF_LINES="$(compute_diff_lines)"
    log "Diff line count (added + deleted): ${DIFF_LINES} / ${MAX_DIFF_LINES}"
    if [ "$DIFF_LINES" -gt "$MAX_DIFF_LINES" ]; then
        log "BLAST RADIUS VIOLATION: Diff lines (${DIFF_LINES}) exceed MAX_DIFF_LINES (${MAX_DIFF_LINES})"
        exit 10
    fi

    git -c core.quotePath=false diff HEAD > "$DIFF_FILE"
    if [ ! -s "$DIFF_FILE" ]; then
        log "No changes produced by coding agent on attempt ${attempt}"
        LAST_FAILURE_CODE=1
        RETRY_FEEDBACK="Your previous attempt produced an empty git diff. Apply a concrete fix."
        continue
    fi

    APPROVED_PATHS=()
    while IFS= read -r -d '' approved_path; do
        [ -n "$approved_path" ] && APPROVED_PATHS+=("$approved_path")
    done < <(git -c core.quotePath=false diff HEAD --name-only -z)
    MODIFIED_FILES="$(git -c core.quotePath=false diff HEAD --name-only)"
    DIFF_STATS="$(git -c core.quotePath=false diff HEAD --stat)"

    if ! "$PYTHON_BIN" "$AGENT_BIN" \
        --mode review \
        --repo-dir "$REPO_DIR" \
        --diff-file "$DIFF_FILE" \
        --base-url "$LLM_BASE_URL" \
        --api-key "$LLM_API_KEY" \
        --model "$CODING_MODEL" 2> "$REVIEW_ERR_FILE"; then
        REVIEW_MSG="$(cat "$REVIEW_ERR_FILE")"
        printf '%s\n' "$REVIEW_MSG" >&2
        log "Adversarial reviewer rejected diff on attempt ${attempt}"
        LAST_FAILURE_CODE=12
        RETRY_FEEDBACK="$(printf 'Adversarial Senior Engineer reviewer rejected your previous diff:\n%s\n\nPrevious diff:\n%s' "$REVIEW_MSG" "$(cat "$DIFF_FILE")")"
        continue
    fi
    cat "$REVIEW_ERR_FILE" >&2 || true

    log "Running regression test gate: ${TEST_COMMAND}"
    if ! ( eval "$TEST_COMMAND" ) > "$TEST_OUT_FILE" 2>&1; then
        cat "$TEST_OUT_FILE" >&2 || true
        LAST_TEST_OUTPUT="$(tail -n 80 "$TEST_OUT_FILE")"
        log "Regression tests failed on attempt ${attempt}"
        LAST_FAILURE_CODE=13
        RETRY_FEEDBACK="$(printf 'Regression tests (%s) failed with output:\n%s\n\nPrevious diff:\n%s' "$TEST_COMMAND" "$LAST_TEST_OUTPUT" "$(cat "$DIFF_FILE")")"
        continue
    fi
    cat "$TEST_OUT_FILE" >&2 || true
    LAST_TEST_OUTPUT="$(tail -n 40 "$TEST_OUT_FILE")"

    log "All gates passed on attempt ${attempt}. Committing and creating PR..."
    ROOT_CAUSE="$(cat "$ROOT_CAUSE_FILE" 2>/dev/null || echo 'See modified files and diff below.')"

    if [ "${#APPROVED_PATHS[@]}" -eq 0 ]; then
        log "ERROR: No approved paths to commit"
        exit 1
    fi
    for approved_path in "${APPROVED_PATHS[@]}"; do
        if [ -e "$approved_path" ] || [ -L "$approved_path" ]; then
            if ! git add -- "$approved_path" >&2 2>&1; then
                log "ERROR: git add failed for ${approved_path}"
                exit 1
            fi
        else
            if ! git rm -q --cached --ignore-unmatch -- "$approved_path" >&2 2>&1; then
                log "ERROR: git rm failed for ${approved_path}"
                exit 1
            fi
        fi
    done
    if ! git commit -m "fix(autopatch): resolve ticket #${TICKET_ID}" >&2 2>&1; then
        log "ERROR: git commit failed"
        exit 1
    fi
    if ! git push -u origin "$BRANCH_NAME" >&2 2>&1; then
        log "ERROR: git push failed"
        exit 1
    fi

    cat > "$PR_BODY_FILE" <<EOF
## User Report
${BUG_PROMPT}

## Root Cause Analysis
${ROOT_CAUSE}

## Modified Files
\`\`\`
${MODIFIED_FILES}
\`\`\`

## Diff Stats
\`\`\`
${DIFF_STATS}
\`\`\`

## Regression Test Verification
Command: \`${TEST_COMMAND}\` (passed on attempt ${attempt}/${MAX_RETRIES})
\`\`\`
${LAST_TEST_OUTPUT}
\`\`\`
EOF

    GH_PR_OUTPUT=""
    if ! GH_PR_OUTPUT="$(gh pr create \
        --base "$BASE_BRANCH" \
        --head "$BRANCH_NAME" \
        --title "fix(autopatch): ticket #${TICKET_ID}" \
        --body-file "$PR_BODY_FILE" 2>&1)"; then
        log "ERROR: gh pr create failed: ${GH_PR_OUTPUT}"
        exit 1
    fi

    printf '%s\n' "$GH_PR_OUTPUT" >&2
    PR_URL="$(printf '%s\n' "$GH_PR_OUTPUT" | grep -Eo 'https?://[^[:space:]]+' | tail -n 1 || true)"
    if [ -z "$PR_URL" ]; then
        PR_URL="$(printf '%s\n' "$GH_PR_OUTPUT" | awk 'NF { line=$0 } END { print line }')"
    fi
    if [ -z "$PR_URL" ]; then
        log "ERROR: Could not extract PR URL from gh output"
        exit 1
    fi

    printf 'PR_URL=%s\n' "$PR_URL"
    exit 0
done

log "Retries exhausted (${MAX_RETRIES}); exiting with code ${LAST_FAILURE_CODE}"
exit "$LAST_FAILURE_CODE"
