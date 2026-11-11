# tower

A local board of agent sessions (Claude Code and Codex) across your git repos. It warns, without blocking, when two worktrees of one repo change the same files.

## How it works

tower is a single Go binary with no daemon. Every invocation is short-lived and reads or writes files in a state directory, then asks git what each worktree changed.

```
Claude Code / Codex ──hooks──▶ tower hook <agent> ──▶ state dir ◀── tower board / check / intent
                                      │                                     │
                                      └──────────── git sweep ◀─────────────┘
```

- **Session records:** each agent session gets one JSON file in `sessions/`, holding its repo, worktree, intent, process ID and last prompt time. A subagent working in a different worktree gets its own record under the same session ID.
- **Liveness:** a session is live while its agent process exists, checked by PID plus process start time, read from `/proc` on Linux and from `ps` on macOS. If no process was found, it counts as live until 12 hours of quiet. Subagents live and end with their coordinator.
- **Changed files:** per worktree, this is uncommitted and untracked files, plus everything committed since it branched from the main checkout's HEAD. Git runs with `GIT_OPTIONAL_LOCKS=0` so it never contends with an agent's own git commands.
- **Overlap:** two worktrees overlap when their changed-file lists intersect. A session's own worktree and its subagents' worktrees are one family and are never compared with each other.
- **Cleanup:** dead records are deleted during sweeps, except the newest one for a worktree that still has changes, which is kept as its "last intent". Repos whose directory is gone are forgotten.
- **State location:** `$TOWER_STATE_DIR`, else `$XDG_STATE_HOME/tower`, else `~/.local/state/tower`. It holds `sessions/`, `repos/`, `events/` and `tower.log`.

## Commands

| Feature | Command | What it does |
|---|---|---|
| Board | `tower` or `tower board` | Lists every known repo with live sessions or unmerged worktrees: worktree, branch, agent, intent, last activity, changed-file count, subagents nested under their session, idle worktrees, and `!` lines for overlaps. |
| Board as JSON | `tower board --json` | Same data as `{"repos": [...]}` for scripts. |
| Overlap check | `tower check` (from inside a worktree) | Prints the full list of files this worktree's family shares with other worktrees, plus other live sessions in the same working tree. |
| Set intent | `tower intent "<one line>"` (inside an agent session) | Sets the session's one-line intent. Needs `CLAUDE_CODE_SESSION_ID` or `CODEX_THREAD_ID` in the environment. |
| Register a subagent worktree | The same `tower intent` command, run from a different worktree than the session's | Creates a subagent record for that worktree under the session. |
| Install hooks | `tower install` | Adds hooks to `~/.claude/settings.json` and `~/.codex/hooks.json`, keeps other entries, and writes a timestamped backup. Codex needs `/hooks` approval afterwards. |
| Uninstall hooks | `tower uninstall` | Removes only tower's hook entries. |
| Usage text | Any unknown command | Prints the command list and exits with status 2. |

## Automatic behavior (via hooks)

These run through `tower hook claude|codex` once hooks are installed. A hook never fails the agent; errors go to `tower.log`.

| Hook event | What happens |
|---|---|
| `SessionStart` | Registers the session and tells the agent about tower and about other live sessions in the same repo. |
| `UserPromptSubmit` | Refreshes the record and takes the first plain-text prompt line as the intent if none is set. It runs an overlap sweep and warns the agent only when the set of overlaps has changed. |
| `SubagentStart` | Tells the subagent it can register its own worktree with `tower intent`. |
| `SessionEnd` | Marks the session and its subagents as ended. |

**Event history:** every start, intent, warn, clear and end is appended to `events/<year>-<month>.jsonl` with branch, HEAD, changed-file count and overlap details. No command reads it; open the files directly.
