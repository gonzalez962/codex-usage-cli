# codex-usage-cli pi extension

A copyable Pi extension that automatically runs [`codex-usage-cli`](../README.md) after every terminal subagent execution, so the active OpenAI/ChatGPT account usage is refreshed (and rotated, if eligible) without you having to remember to run the CLI by hand.

It integrates with [`pi-subagents-j0k3r`](https://www.npmjs.com/package/pi-subagents-j0k3r) so that:

- Synchronous `task`-mode `subagent_run` completions trigger a refresh.
- Asynchronous `background`-mode `subagent_run` launches trigger a refresh when their completion custom message arrives later.
- `subagent_continue` task-mode completions trigger a refresh.

Output is shown through UI notifications only and is never injected into the LLM context.

## Prerequisites

- Pi with the documented `tool_result`, `message_start`, and `pi.exec` runtime methods available.
- The [`codex-usage-cli`](../README.md) binary on your `PATH` (or in the directory tree Pi resolves through `PATH`). The extension calls it as `codex-usage-cli` with no arguments so it prints the active account percentage and may rotate if the configured threshold is exceeded.
- The companion `pi-subagents-j0k3r` package installed in Pi (the extension reads the subagent completion custom message that package emits).

No additional npm packages, no Go toolchain, no shell scripts. The extension is a single TypeScript file at `pi-plugin/index.ts`.

## Install (copy, no package registry)

This folder is a copyable extension. Pick the destination that matches the scope you want:

### Global (every project)

```text
~/.pi/agent/extensions/codex-usage-cli/index.ts
```

Copy the contents of `pi-plugin/` (or just `pi-plugin/index.ts`) into that directory so Pi loads it on every session start.

### Project-local (this workspace only)

```text
.pi/extensions/codex-usage-cli/index.ts
```

The package layout is otherwise identical to the global copy. Choose global when you want the refresh behaviour everywhere you use Pi; choose project-local when only this workspace should refresh accounts after subagent work.

### Reload Pi

After either copy, restart Pi or run `/reload` from inside an existing session so the extension is loaded.

## What it does

The extension does **not** invoke `codex-usage-cli` at load time or on `session_start`. Loading Pi never rotates or synchronises accounts. The CLI is invoked as a post-completion side effect after subagent work and, exactly once per Pi session, immediately before the first main-agent user query:

1. On `tool_result` for `subagent_run`, it reads `event.details` directly. If `details.results` is a non-empty array, each entry is filtered before it can queue a refresh: the entry must (a) be identified as a synchronous member via `details.waited_task_ids` when that field is present on the details payload, (b) carry a terminal task status (`completed`, `failed`, `cancelled`, or `interrupted`), and (c) not have `effective_mode` (or fallback `mode`) of `background`. Entries that fail any of those guards are skipped. Background launches do **not** populate `details.results` in the pure-background branch and therefore never queue anything here; they finish later through the completion custom message.
2. On `tool_result` for `subagent_continue`, it reads `event.details` directly. Task-mode continuations populate `details.results` and queue a refresh after the same terminal / non-background filter is applied (the package populates `results` only for task-mode continuations, so the filter is a defensive guard, not a behavioural change for the common path); background-handoff continuations omit `results` and are skipped here.
3. On `message_start` whose `message.customType === "subagent-completion"`, it reads `message.details.task` and queues a refresh. No terminal / mode filter is applied here on purpose: the `subagent-completion` custom message is the runtime's authoritative notification for a finished background (or post-handoff) member, and its `details.task` already represents a terminal completion.

Invocations are serialized through a single FIFO queue so two completions never race on the shared credential store and cannot trigger concurrent rotation. Each call uses a bounded `pi.exec` `timeout` (8 s) and propagates `ctx.signal` so the CLI is cancelled if the parent turn is cancelled. The extension never reads `event.result.details` (the hook payload already exposes the metadata at the top level).

## First main-agent query

In addition to the post-completion hooks, the extension runs `codex-usage-cli` **exactly once per Pi session**, on the **first** main-agent user query, so the active account is fresh before the very first model request:

1. A Pi `input` hook fires before agent processing on every user input.
2. The hook triggers only when `event.source === "interactive"` (typed at the TUI prompt) or `event.source === "rpc"` (sent through the RPC interface). Inputs whose source is `"extension"` are intentionally **not counted**, so extension-generated turns never trigger the first-time refresh. Extension commands such as `/reload` are handled before the `input` hook and do not count as the first query.
3. The first eligible input flips a session-local guard to `true` **before** scheduling, so a concurrent or reentrant input cannot enqueue a second refresh.
4. The handler `await`s the serialized `schedule` queue, so the live usage refresh and any account rotation complete before the main-model request is sent. Because `pi.exec` uses an 8 s timeout, the first query of a Pi session may be delayed by up to 8 seconds.
5. The UI notification is labelled `codex-usage-cli: main initial-query → X% used.` so the main-agent refresh is easy to distinguish from the subagent post-completion notifications.
6. After the first eligible user query the guard stays `true` for the rest of the Pi session, so subsequent main-agent queries do **not** re-run the CLI.

`/reload` re-evaluates the extension and clears its in-memory guard. Once reload finishes, the **next** interactive or RPC input receives a new initial check. Subsequent subagent completions continue to use the same post-completion hooks as before.

Output is shown through UI notifications only and is never injected into the LLM context, and the CLI itself owns all credential access — including any rotation or synchronization triggered by running above its threshold; see the warning in [Rotation and credential side effects](#rotation-and-credential-side-effects).

## Task vs background behaviour

| Subagent path | Where the refresh is queued |
|---|---|
| `subagent_run` mode `task` | `tool_result` hook (sync, `details.results` populated, all entries pass the filter) |
| `subagent_run` mode `background` | `message_start` hook when `subagent-completion` arrives |
| `subagent_run` mode `mixed` | `tool_result` hook queues **only waited task-mode members** (terminal, non-background); background members are deferred to `message_start` so the later `subagent-completion` is the single refresh source for them |
| `subagent_continue` mode `task` | `tool_result` hook (sync, `details.results` populated) |
| `subagent_continue` mode `background` | `message_start` hook when `subagent-completion` arrives |

A task-mode continuation that hands off to the background mid-run (via `ctrl+h` by default) finishes through the completion message; the extension handles that path the same way as a background launch.

## Mixed-mode `subagent_run`

When a single `subagent_run` resolves as `mode: "mixed"` (some members waited synchronously, others were launched in the background), `event.details.results` may include **both** kinds of members. The `tool_result` handler applies three guards before enqueueing a refresh:

- `details.waited_task_ids` is preferred when present. Only entries whose `id` appears in that set pass; anything else is rejected even if it already reached a terminal status.
- The entry's `status` must be terminal: `completed`, `failed`, `cancelled`, or `interrupted`. Entries in `queued`, `running`, or `stopping` are rejected.
- The entry's `effective_mode` (falling back to `mode`) must not be `background`, even if it already reached a terminal status before the mixed tool result returned. This guarantees that the later `subagent-completion` notification is the single refresh source for background members.

When `details.waited_task_ids` is absent (for example on a `subagent_continue` task-mode result), the synchronous-membership guard is skipped and only the terminal / non-background guards apply. The `message_start` handler for `subagent-completion` does not re-apply these filters so background metadata is preserved and forwarded to the queue exactly as the runtime emitted it.

## Rotation and credential side effects

Every refresh invokes:

```bash
codex-usage-cli
```

with no arguments. That prints only the `used_percent` of the active account (for example `64`), and the extension validates that the stdout is a finite number before notifying you.

If the active account usage is above the CLI's configured threshold (the 5-hour window at 80% or the weekly window at 98% when the toggle is on; the weekly window at 98% when the toggle is off), the CLI may rotate to another eligible account and/or synchronise the credential store as a side effect. This is intentional: the extension runs the same command you would run by hand, just after each subagent turn rather than at session boundaries. **Running `codex-usage-cli` directly from a prompt or a bare terminal invocation has the same rotation semantics** — the CLI is the single source of truth for rotation, not the extension. Account changes happen entirely inside the CLI process; the extension never reads, writes, or prints credential material.

## Security

- The extension runs `codex-usage-cli` through `pi.exec` only. It never invokes a shell or writes credential files directly; any documented account synchronization is performed by the CLI itself.
- CLI stderr is never surfaced in notifications or model context. Non-zero exits, missing binaries, malformed output, and non-numeric stdout produce only a short UI warning.
- `event.details` and `message.details` are read only for safe scalar fields (`id`, `agent`, `status`, `mode`, `effective_mode`, plus the top-level `waited_task_ids` array on tool results). Result bodies, error metadata, transcripts, thread snapshots, and any token / auth material are intentionally ignored.
- The extension does not read `event.result.details`, OpenCode auth files, or environment variables. The CLI itself owns all credential access.

## No `subagents.json` change needed

The extension does not read or modify `subagents.json`. It does not change `default_mode`, `enable_continue`, model profiles, tool allowlists, shortcuts, history settings, or any other subagent configuration. The only thing it does is react to hooks the existing subagent package already emits. Drop the extension in, reload, and the refresh behaviour is active.

## Skill vs extension

The companion `codex-usage-cli` skill shipped at [`skills/codex-usage-cli/SKILL.md`](../skills/codex-usage-cli/SKILL.md) is **instructional context for the model**. It tells the model when it is appropriate to invoke the CLI, what each output column means, and how to summarise results safely. The model only uses that skill when it explicitly wants to query usage from a prompt.

This extension is the **lifecycle automation**. It runs the same CLI on a hook, after subagent work, without involving the model. The skill and the extension are complementary: the skill teaches the model to read usage; the extension keeps usage fresh in the background so the model does not have to ask.

## Uninstall

Remove the copy from whichever scope you chose:

```bash
rm -rf ~/.pi/agent/extensions/codex-usage-cli
# or, for project-local:
rm -rf .pi/extensions/codex-usage-cli
```

Then run `/reload` (or restart Pi). The extension leaves no state behind: no files outside `pi-plugin/`, no environment variables, no persistent cache.