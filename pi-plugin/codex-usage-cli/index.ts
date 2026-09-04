import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const CLI = "codex-usage-cli";
const TIMEOUT_MS = 8_000;
const TERMINAL_STATUSES = new Set([
	"completed",
	"failed",
	"cancelled",
	"interrupted",
]);

type TaskMeta = {
	id?: string;
	agent?: string;
	status?: string;
	mode?: string;
	effective_mode?: string;
};

type ToolDetails = {
	results?: unknown[];
	waited_task_ids?: unknown[];
};

export default function codexUsageOnSubagent(pi: ExtensionAPI): void {
	// Session-local guard for the first main-agent user query; flipped before scheduling
	// so concurrent/reentrant inputs cannot enqueue a second first-time refresh. A `/reload`
	// re-imports this module and clears the flag.
	let firstInputQueued = false;

	// Serialize checks because the CLI may rotate and synchronize credentials.
	let queue: Promise<void> = Promise.resolve();

	function notify(ctx: any, message: string, level: "info" | "warning" = "info"): void {
		try {
			ctx?.ui?.notify?.(message, level);
		} catch {
			// Notifications are best-effort and never enter model context.
		}
	}

	function schedule(ctx: any, task?: TaskMeta): Promise<void> {
		const label = [task?.agent ?? "subagent", task?.status, task?.id]
			.filter(Boolean)
			.join(" ");

		const check = async (): Promise<void> => {
			try {
				const result = await pi.exec(CLI, [], {
					timeout: TIMEOUT_MS,
					signal: ctx?.signal,
				});
				const output = result.stdout.trim();
				const percentage = Number(output);

				if (
					result.code !== 0
					|| output === ""
					|| !Number.isFinite(percentage)
					|| percentage < 0
					|| percentage > 100
				) {
					notify(ctx, `codex-usage-cli: check failed after ${label}.`, "warning");
					return;
				}

				notify(ctx, `codex-usage-cli: ${label} → ${output}% used.`);
			} catch {
				notify(ctx, `codex-usage-cli: unavailable after ${label}.`, "warning");
			}
		};

		// The rejection branch keeps later checks running if an unexpected error escapes.
		// The returned Promise is awaited by the first-input handler so rotation completes
		// before the first main-model request; tool_result/message_start callers ignore it.
		return queue = queue.then(check, check);
	}

	// Synchronous task-mode completions arrive in event.details.results.
	pi.on("tool_result", (event: any, ctx: any) => {
		if (event?.toolName !== "subagent_run" && event?.toolName !== "subagent_continue") {
			return;
		}

		const details = event.details as ToolDetails | undefined;
		if (!Array.isArray(details?.results) || details.results.length === 0) return;

		const waitedIds = Array.isArray(details.waited_task_ids)
			? new Set(details.waited_task_ids.filter((id): id is string => typeof id === "string"))
			: undefined;

		for (const value of details.results) {
			const task = readTask(value);
			if (!task || !task.status || !TERMINAL_STATUSES.has(task.status)) continue;
			if ((task.effective_mode ?? task.mode) === "background") continue;
			if (waitedIds && (!task.id || !waitedIds.has(task.id))) continue;
			schedule(ctx, task);
		}
	});

	// Background completions are emitted later as a custom message.
	pi.on("message_start", (event: any, ctx: any) => {
		const message = event?.message;
		if (message?.customType !== "subagent-completion") return;
		schedule(ctx, readTask(message?.details?.task));
	});

	// First main-agent user query per Pi session: refresh once before the first model request.
	pi.on("input", async (event: any, ctx: any) => {
		if (firstInputQueued) return;
		if (event?.source !== "interactive" && event?.source !== "rpc") return;
		firstInputQueued = true;
		await schedule(ctx, { agent: "main", status: "initial-query" });
	});
}

function readTask(value: unknown): TaskMeta | undefined {
	if (!value || typeof value !== "object") return undefined;
	const source = value as Record<string, unknown>;
	const task: TaskMeta = {};

	for (const key of ["id", "agent", "status", "mode", "effective_mode"] as const) {
		if (typeof source[key] === "string") task[key] = source[key];
	}

	return Object.keys(task).length > 0 ? task : undefined;
}
