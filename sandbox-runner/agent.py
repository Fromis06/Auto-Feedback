#!/usr/bin/env python3
"""Stdlib-only headless coding and adversarial diff review agent."""

import argparse
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

REVIEW_SYSTEM_PROMPT = (
    "Act as a hostile Senior Engineer. Check if this diff breaks existing API "
    "contracts, alters shared state unexpectedly, or introduces side effects. "
    "Reply PASS or FAIL: ."
)

CODE_SYSTEM_PROMPT = (
    "You are an automated senior software engineer fixing a bug in a git repository.\n"
    "Produce minimal, surgical fixes that address the root cause without breaking APIs "
    "or touching protected paths.\n"
    "Respond ONLY with a valid JSON object matching this schema:\n"
    "{\n"
    '  "root_cause": "concise technical explanation of the root cause and fix",\n'
    '  "edits": [\n'
    "    {\n"
    '      "path": "relative/path/to/file",\n'
    '      "action": "replace",\n'
    '      "search": "exact existing code snippet to replace",\n'
    '      "replace": "new code snippet"\n'
    "    },\n"
    "    {\n"
    '      "path": "relative/path/to/new_or_updated_file",\n'
    '      "action": "write",\n'
    '      "content": "complete file contents"\n'
    "    },\n"
    "    {\n"
    '      "path": "relative/path/to/remove",\n'
    '      "action": "delete"\n'
    "    }\n"
    "  ]\n"
    "}\n"
    "Prefer targeted 'replace' edits for existing files. Only use 'write' for new files "
    "or small files. Do not include markdown prose outside the JSON object."
)

SKIP_DIRS = {
    ".git",
    "node_modules",
    "dist",
    "build",
    "__pycache__",
    ".next",
    ".venv",
    "venv",
    "vendor",
    "target",
}

MAX_FILE_BYTES = 24_000
MAX_CONTEXT_CHARS = 60_000
MAX_DIFF_CHARS = 40_000


def log(msg: str) -> None:
    print(f"[autopatch-agent] {msg}", file=sys.stderr, flush=True)


def run_git(repo_dir: Path, args: list[str]) -> str:
    proc = subprocess.run(
        ["git", "-c", "core.quotePath=false", *args],
        cwd=str(repo_dir),
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    if proc.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {proc.stderr.strip()}")
    return proc.stdout


def call_chat_completions(
    base_url: str,
    api_key: str,
    model: str,
    system_prompt: str,
    user_prompt: str,
    timeout: int = 120,
) -> str:
    url = base_url.rstrip("/") + "/chat/completions"
    payload = {
        "model": model,
        "temperature": 0.1,
        "messages": [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": user_prompt},
        ],
    }
    headers = {"Content-Type": "application/json"}
    if api_key.strip():
        headers["Authorization"] = f"Bearer {api_key.strip()}"

    req = urllib.request.Request(
        url,
        data=json.dumps(payload).encode("utf-8"),
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace") if exc.fp else ""
        raise RuntimeError(f"LLM HTTP {exc.code}: {body[:500]}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(f"LLM connection error: {exc.reason}") from exc

    try:
        data = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"Invalid JSON from LLM endpoint: {raw[:300]}") from exc

    usage = data.get("usage")
    if isinstance(usage, dict) and "total_tokens" in usage:
        log(f"tokens_used={usage['total_tokens']}")

    choices = data.get("choices")
    if not isinstance(choices, list) or not choices:
        raise RuntimeError(f"LLM response missing choices: {raw[:300]}")

    message = choices[0].get("message", {})
    content = message.get("content")
    if not isinstance(content, str) or not content.strip():
        raise RuntimeError("LLM returned empty message content")
    return content


def is_probably_text(path: Path) -> bool:
    try:
        chunk = path.read_bytes()[:4096]
    except OSError:
        return False
    return b"\x00" not in chunk


def collect_repo_files(repo_dir: Path, prompt_text: str) -> tuple[list[str], str]:
    try:
        tracked_out = run_git(repo_dir, ["ls-files"])
        candidates = [line.strip() for line in tracked_out.splitlines() if line.strip()]
    except RuntimeError:
        candidates = []

    if not candidates:
        for root, dirs, files in os.walk(repo_dir):
            dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
            for name in files:
                rel = (Path(root) / name).relative_to(repo_dir).as_posix()
                candidates.append(rel)

    filtered: list[str] = []
    for rel in sorted(set(candidates)):
        parts = Path(rel).parts
        if any(p in SKIP_DIRS for p in parts):
            continue
        filtered.append(rel)

    lower_prompt = prompt_text.lower()

    def score(rel: str) -> tuple[int, int, str]:
        name = Path(rel).name.lower()
        mentioned = 0 if (rel.lower() in lower_prompt or name in lower_prompt) else 1
        priority_ext = (
            0
            if Path(rel).suffix.lower()
            in {".js", ".ts", ".jsx", ".tsx", ".py", ".go", ".java", ".rb", ".rs", ".json", ".yaml", ".yml"}
            else 1
        )
        return (mentioned, priority_ext, rel)

    ordered = sorted(filtered, key=score)
    snippets: list[str] = []
    total_chars = 0

    for rel in ordered:
        full = repo_dir / rel
        if not full.is_file() or not is_probably_text(full):
            continue
        try:
            text = full.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        if len(text) > MAX_FILE_BYTES:
            text = text[:MAX_FILE_BYTES] + "\n... [truncated] ..."
        block = f"=== FILE: {rel} ===\n{text}\n"
        if total_chars + len(block) > MAX_CONTEXT_CHARS:
            break
        snippets.append(block)
        total_chars += len(block)

    return filtered, "\n".join(snippets)


def extract_json_object(raw: str) -> dict:
    cleaned = raw.strip()
    fence_match = re.search(r"```(?:json)?\s*(\{.*\})\s*```", cleaned, re.DOTALL)
    if fence_match:
        cleaned = fence_match.group(1)
    else:
        start = cleaned.find("{")
        end = cleaned.rfind("}")
        if start != -1 and end != -1 and end > start:
            cleaned = cleaned[start : end + 1]
    try:
        parsed = json.loads(cleaned)
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"Coding model did not return valid JSON: {exc}: {raw[:300]}") from exc
    if not isinstance(parsed, dict):
        raise RuntimeError("Coding model JSON root must be an object")
    return parsed


def safe_repo_path(repo_dir: Path, rel_path: str) -> Path:
    if not isinstance(rel_path, str) or not rel_path.strip():
        raise RuntimeError("Edit entry missing valid path")
    candidate = Path(rel_path.strip())
    if candidate.is_absolute():
        raise RuntimeError(f"Refusing absolute path in edit: {rel_path}")
    resolved_repo = repo_dir.resolve()
    resolved_target = (resolved_repo / candidate).resolve()
    if resolved_repo == resolved_target or resolved_repo not in resolved_target.parents:
        raise RuntimeError(f"Refusing path outside repository: {rel_path}")
    if ".git" in resolved_target.relative_to(resolved_repo).parts:
        raise RuntimeError(f"Refusing to modify .git metadata: {rel_path}")
    return resolved_target


def apply_edits(repo_dir: Path, payload: dict) -> int:
    edits = payload.get("edits")
    if not isinstance(edits, list) or not edits:
        raise RuntimeError("Coding model JSON contains no edits")

    applied = 0
    for idx, edit in enumerate(edits):
        if not isinstance(edit, dict):
            raise RuntimeError(f"Edit #{idx} is not an object")
        rel_path = edit.get("path", "")
        action = str(edit.get("action", "write")).strip().lower()
        target = safe_repo_path(repo_dir, rel_path)

        if action == "delete":
            if target.exists():
                target.unlink()
                log(f"Deleted {rel_path}")
                applied += 1
        elif action == "write":
            content = edit.get("content")
            if not isinstance(content, str):
                raise RuntimeError(f"Edit write for {rel_path} missing string 'content'")
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(content, encoding="utf-8")
            log(f"Wrote {rel_path} ({len(content)} bytes)")
            applied += 1
        elif action == "replace":
            search = edit.get("search")
            replace = edit.get("replace")
            if not isinstance(search, str) or not isinstance(replace, str):
                raise RuntimeError(f"Edit replace for {rel_path} missing 'search'/'replace'")
            if not target.is_file():
                raise RuntimeError(f"Cannot replace in missing file: {rel_path}")
            original = target.read_text(encoding="utf-8", errors="replace")
            if search in original:
                updated = original.replace(search, replace, 1)
            else:
                norm_orig = original.replace("\r\n", "\n")
                norm_search = search.replace("\r\n", "\n")
                if norm_search not in norm_orig:
                    raise RuntimeError(f"Search block not found in {rel_path}")
                updated = norm_orig.replace(norm_search, replace, 1)
            target.write_text(updated, encoding="utf-8")
            log(f"Patched {rel_path}")
            applied += 1
        else:
            raise RuntimeError(f"Unsupported edit action '{action}' for {rel_path}")

    return applied


def run_code_mode(args: argparse.Namespace) -> int:
    repo_dir = Path(args.repo_dir).resolve()
    prompt_text = Path(args.prompt_file).read_text(encoding="utf-8") if args.prompt_file else (args.prompt or "")
    if not prompt_text.strip():
        log("Empty prompt provided in code mode")
        return 1

    file_list, repo_context = collect_repo_files(repo_dir, prompt_text)
    current_diff = ""
    try:
        current_diff = run_git(repo_dir, ["diff", "HEAD"]).strip()
    except RuntimeError:
        current_diff = ""

    parts = [
        "=== REPOSITORY FILES ===\n" + "\n".join(file_list[:300]),
        "=== FILE CONTENTS ===\n" + repo_context,
        "=== TASK & BUG CONTEXT ===\n" + prompt_text.strip(),
    ]
    if current_diff:
        parts.append("=== CURRENT WORKING DIFF ===\n" + current_diff[:MAX_DIFF_CHARS])
    if args.feedback and args.feedback.strip():
        parts.append("=== PREVIOUS ATTEMPT FEEDBACK (FIX THESE ISSUES) ===\n" + args.feedback.strip())

    user_prompt = "\n\n".join(parts)
    log(f"Calling coding model '{args.model}' at {args.base_url}")
    reply = call_chat_completions(
        base_url=args.base_url,
        api_key=args.api_key,
        model=args.model,
        system_prompt=CODE_SYSTEM_PROMPT,
        user_prompt=user_prompt,
    )

    payload = extract_json_object(reply)
    applied = apply_edits(repo_dir, payload)
    if applied == 0:
        log("No edits were applied")
        return 1

    if args.summary_out:
        root_cause = str(payload.get("root_cause", "")).strip() or "Automated fix applied based on telemetry."
        Path(args.summary_out).write_text(root_cause, encoding="utf-8")

    return 0


def run_review_mode(args: argparse.Namespace) -> int:
    repo_dir = Path(args.repo_dir).resolve()
    if args.diff_file:
        diff_text = Path(args.diff_file).read_text(encoding="utf-8", errors="replace")
    elif not sys.stdin.isatty():
        diff_text = sys.stdin.read()
    else:
        diff_text = run_git(repo_dir, ["diff", "HEAD"])

    if not diff_text.strip():
        log("FAIL: Empty diff cannot be approved")
        return 1

    if len(diff_text) > MAX_DIFF_CHARS:
        diff_text = diff_text[:MAX_DIFF_CHARS] + "\n... [diff truncated] ..."

    log(f"Calling adversarial reviewer model '{args.model}' at {args.base_url}")
    reply = call_chat_completions(
        base_url=args.base_url,
        api_key=args.api_key,
        model=args.model,
        system_prompt=REVIEW_SYSTEM_PROMPT,
        user_prompt=diff_text,
    ).strip()

    first_line = reply.splitlines()[0].strip() if reply else ""
    if re.match(r"^PASS\b", first_line, re.IGNORECASE) and not re.search(r"\bFAIL\b", first_line, re.IGNORECASE):
        log(f"Reviewer decision: {reply}")
        return 0

    log(f"Reviewer rejected diff: {reply}")
    return 1


def main() -> int:
    parser = argparse.ArgumentParser(description="AutoPatch stdlib coding/review agent")
    parser.add_argument("--mode", required=True, choices=["code", "review"])
    parser.add_argument("--repo-dir", default=".")
    parser.add_argument("--prompt-file", default="")
    parser.add_argument("--prompt", default="")
    parser.add_argument("--feedback", default="")
    parser.add_argument("--diff-file", default="")
    parser.add_argument("--summary-out", default="")
    parser.add_argument("--base-url", default=os.environ.get("LLM_BASE_URL", "https://api.openai.com/v1"))
    parser.add_argument("--api-key", default=os.environ.get("LLM_API_KEY", ""))
    parser.add_argument("--model", default=os.environ.get("CODING_MODEL", "gpt-4o"))
    args = parser.parse_args()

    try:
        if args.mode == "code":
            return run_code_mode(args)
        return run_review_mode(args)
    except Exception as exc:
        log(f"Runtime error: {exc}")
        return 1


if __name__ == "__main__":
    sys.exit(main())
