#!/usr/bin/env python3
"""UserPromptSubmit / SessionStart hook: derive a short session title for
the sidebar-go card whenever the user submits a prompt.

Output: ~/.local/state/tmux-sidebar/context/title-<session_id>.json

Why a file (not OSC 2): Claude Code's TUI repaints #{pane_title} every
~500ms-1s with its own frozen session summary, so OSC 2 writes from a
hook lose the race. sidebar-go reads this file as the title source of
truth (with pane.Title as fallback).

Two-stage pipeline:
  * foreground (default): parse hook JSON, write stage-1 placeholder so
    sidebar-go has *something* immediately, then detach a stage-2 worker
    and exit (typically <50ms).
  * --bg: detached worker calls Groq LLM to generate a 3-6 word session
    title, atomic-rewrites the JSON file.

Install: copy to ~/.claude/hooks/ and register in settings.json
Config: settings.json hooks.UserPromptSubmit + hooks.SessionStart, async: true, timeout: 2
Requires: GROQ_API_KEY in environment or in a .env file next to this script
"""

import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

HOME = Path.home()
STATE_DIR = Path(
    os.environ.get("TMUX_SIDEBAR_STATE_DIR")
    or os.path.join(
        os.environ.get("XDG_STATE_HOME", str(HOME / ".local/state")),
        "tmux-sidebar",
    )
)
OUT_DIR = STATE_DIR / "context"
ENV_FILE = Path(__file__).resolve().parent / ".env"
GROQ_URL = "https://api.groq.com/openai/v1/chat/completions"
DEFAULT_MODEL = "llama-3.1-8b-instant"

SYS_PROMPT = (
    "You generate 3-6 word imperative titles for TUI tabs that label a "
    "coding session. Track the conversation arc - bias toward what the "
    "user is currently working on (newest prompt), but use the recent "
    "messages for context if the newest prompt is a follow-up question. "
    'Output ONLY the title - no quotes, no period, no prefix. Examples: '
    '"Fix sidebar footer bug", "Audit Go memory profile", '
    '"Draft release notes".'
)


def now_ms() -> int:
    return int(time.time() * 1000)


def write_atomic(path: Path, payload: dict) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(payload, ensure_ascii=False, indent=2))
    tmp.replace(path)


def load_groq_key() -> tuple[str | None, str | None]:
    key = os.environ.get("GROQ_API_KEY")
    if key:
        return key, None
    if not ENV_FILE.is_file():
        return None, f"env file missing: {ENV_FILE}"
    try:
        for line in ENV_FILE.read_text().splitlines():
            line = line.strip()
            if line.startswith("GROQ_API_KEY="):
                v = line.split("=", 1)[1].strip().strip('"').strip("'")
                return (v, None) if v else (None, "GROQ_API_KEY empty")
    except OSError as e:
        return None, f"env read error: {e}"
    return None, f"GROQ_API_KEY not in {ENV_FILE}"


def recent_user_messages(transcript_path: str, n: int = 3) -> str:
    if not transcript_path:
        return ""
    p = Path(transcript_path)
    if not p.is_file():
        return ""
    try:
        with p.open("rb") as f:
            f.seek(0, os.SEEK_END)
            size = f.tell()
            chunk = min(size, 256 * 1024)
            f.seek(size - chunk)
            tail = f.read().decode("utf-8", errors="ignore")
    except OSError:
        return ""
    msgs: list[str] = []
    for line in tail.splitlines()[-200:]:
        try:
            rec = json.loads(line)
        except json.JSONDecodeError:
            continue
        if rec.get("type") != "user":
            continue
        content = rec.get("message", {}).get("content")
        if not isinstance(content, str):
            continue
        if content.startswith(("<local-command", "<system-reminder", "<command-name")):
            continue
        msgs.append(content.strip())
    return " | ".join(msgs[-n:])[:1200]


def call_groq(api_key: str, model: str, user_ctx: str, timeout: int = 5):
    request_body = {
        "model": model,
        "messages": [
            {"role": "system", "content": SYS_PROMPT},
            {"role": "user", "content": user_ctx},
        ],
        "temperature": 0,
        "max_tokens": 30,
    }
    req = urllib.request.Request(
        GROQ_URL,
        data=json.dumps(request_body).encode(),
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
            "User-Agent": "curl/8.4.0",
        },
    )
    t0 = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
        latency_ms = int((time.monotonic() - t0) * 1000)
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError as e:
            return None, request_body, latency_ms, f"groq: non-JSON response: {e}: {raw[:200]}"
        if isinstance(parsed, dict) and parsed.get("error"):
            msg = parsed["error"].get("message", "unknown")
            return parsed, request_body, latency_ms, f"groq error: {msg}"
        return parsed, request_body, latency_ms, None
    except urllib.error.HTTPError as e:
        latency_ms = int((time.monotonic() - t0) * 1000)
        body = ""
        try:
            body = e.read().decode("utf-8", errors="ignore")[:500]
        except Exception:
            pass
        return None, request_body, latency_ms, f"groq HTTP {e.code}: {body}"
    except urllib.error.URLError as e:
        latency_ms = int((time.monotonic() - t0) * 1000)
        return None, request_body, latency_ms, f"groq URL error: {e.reason}"
    except Exception as e:
        latency_ms = int((time.monotonic() - t0) * 1000)
        return None, request_body, latency_ms, f"groq exception: {type(e).__name__}: {e}"


def extract_title(resp_json) -> str:
    if not isinstance(resp_json, dict):
        return ""
    try:
        text = resp_json["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError):
        return ""
    if not isinstance(text, str):
        return ""
    return text.replace('"', "").strip().rstrip(".").strip()[:60]


def base_record(hook_input: dict, hook_start_ms: int) -> dict:
    prompt = hook_input.get("prompt", "")
    return {
        "stage": "pending",
        "title": prompt[:60],
        "fallback": prompt[:60],
        "ts": int(time.time()),
        "hook_start_ms": hook_start_ms,
        "total_elapsed_ms": None,
        "session_id": hook_input.get("session_id"),
        "cwd": hook_input.get("cwd"),
        "prompt": prompt,
        "transcript_path": hook_input.get("transcript_path"),
        "model": None,
        "latency_ms": None,
        "context": None,
        "request": None,
        "response": None,
        "error": None,
    }


def out_path(sid: str) -> Path:
    return OUT_DIR / f"title-{sid}.json"


def stage2(hook_input: dict, hook_start_ms: int) -> None:
    rec = base_record(hook_input, hook_start_ms)
    sid = rec["session_id"]
    if not sid:
        return
    p = out_path(sid)

    api_key, key_err = load_groq_key()
    if key_err:
        rec["stage"] = "error"
        rec["error"] = key_err
        rec["total_elapsed_ms"] = now_ms() - hook_start_ms
        write_atomic(p, rec)
        return

    model = os.environ.get("INTENT_TITLE_MODEL", DEFAULT_MODEL)
    ctx = recent_user_messages(rec["transcript_path"] or "")
    user_ctx = f"Recent messages: {ctx or '(none)'}\nNewest prompt: {rec['prompt'][:1500]}"

    resp, request_body, latency_ms, groq_err = call_groq(api_key, model, user_ctx)
    title = extract_title(resp) if resp else ""
    err = groq_err
    if not title and not err:
        err = "groq: empty title in response"
    if not title:
        title = rec["fallback"]

    rec.update(
        stage="complete" if err is None else "error",
        title=title,
        model=model,
        latency_ms=latency_ms,
        context=user_ctx,
        request=request_body,
        response=resp,
        error=err,
        ts=int(time.time()),
        total_elapsed_ms=now_ms() - hook_start_ms,
    )
    write_atomic(p, rec)


def stage1(hook_input: dict, hook_start_ms: int) -> None:
    sid = hook_input.get("session_id")
    if not sid:
        return
    rec = base_record(hook_input, hook_start_ms)
    rec["error"] = "groq pending"
    write_atomic(out_path(sid), rec)


def detach_worker(hook_input: dict) -> None:
    try:
        script = str(Path(__file__).resolve())
        proc = subprocess.Popen(
            [sys.executable, script, "--bg"],
            stdin=subprocess.PIPE,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
            close_fds=True,
        )
        assert proc.stdin is not None
        proc.stdin.write(json.dumps(hook_input).encode())
        proc.stdin.close()
    except Exception:
        pass


def run_bg() -> None:
    try:
        hook_input = json.loads(sys.stdin.read())
    except (json.JSONDecodeError, OSError):
        return
    hook_start_ms = int(hook_input.get("_hook_start_ms") or now_ms())
    sid = hook_input.get("session_id")
    try:
        stage2(hook_input, hook_start_ms)
    except Exception as e:
        if not sid:
            return
        rec = base_record(hook_input, hook_start_ms)
        rec.update(
            stage="error",
            error=f"stage2 crashed: {type(e).__name__}: {e}",
            total_elapsed_ms=now_ms() - hook_start_ms,
        )
        try:
            write_atomic(out_path(sid), rec)
        except Exception:
            pass


def sweep_stale_files(ttl_days: int = 7) -> None:
    if not OUT_DIR.is_dir():
        return
    cutoff = time.time() - ttl_days * 86400
    try:
        for p in OUT_DIR.glob("title-*.json"):
            try:
                if p.stat().st_mtime < cutoff:
                    p.unlink()
            except OSError:
                continue
    except OSError:
        pass


def handle_session_start(hook_input: dict) -> None:
    sid = hook_input.get("session_id")
    if not sid:
        return
    source = (hook_input.get("source") or "").lower()
    if source != "clear":
        return
    rec = base_record(hook_input, now_ms())
    rec.update(
        stage="complete",
        title="Ready",
        fallback="Ready",
        error=None,
    )
    try:
        write_atomic(out_path(sid), rec)
    except OSError:
        pass


def run_foreground() -> None:
    hook_start_ms = now_ms()
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    sweep_stale_files()

    try:
        raw = sys.stdin.read()
        hook_input = json.loads(raw)
    except (json.JSONDecodeError, OSError):
        return

    event = hook_input.get("hook_event_name") or ""
    if event == "SessionStart":
        handle_session_start(hook_input)
        return
    if event and event != "UserPromptSubmit":
        return

    sid = hook_input.get("session_id")
    prompt = hook_input.get("prompt")
    if not sid or not prompt:
        return
    hook_input["prompt"] = prompt.replace("\n", " ").replace("\r", " ").replace("\t", " ")
    hook_input["_hook_start_ms"] = hook_start_ms
    stage1(hook_input, hook_start_ms)
    detach_worker(hook_input)


def main() -> None:
    if "--bg" in sys.argv:
        run_bg()
    else:
        run_foreground()


if __name__ == "__main__":
    main()
