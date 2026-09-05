// @ts-expect-error -- Pi provides this module when it loads the extension.
import type {
	ExtensionAPI,
	ExtensionContext,
} from "@earendil-works/pi-coding-agent";

const STATUS_KEY = "codex-usage";
const USAGE_URL = "https://chatgpt.com/backend-api/wham/usage";
const RESET_CREDITS_URL =
	"https://chatgpt.com/backend-api/wham/rate-limit-reset-credits";
const WEEK_SECONDS = 7 * 24 * 60 * 60;
const REFRESH_MS = 5 * 60 * 1000;

type Window = {
	used_percent: number;
	limit_window_seconds: number;
	reset_at: number;
};

type UsagePayload = {
	rate_limit?: {
		primary_window?: Window;
		secondary_window?: Window;
	};
	account?: { email?: string; plan?: string };
};

type Claims = {
	email?: string;
	"https://api.openai.com/auth"?: {
		chatgpt_account_id?: string;
		chatgpt_plan_type?: string;
	};
};

type Status = {
	account?: { email?: string; plan?: string };
	primary?: Window;
	weekly?: Window;
};

type ResetCredit = {
	id: string;
	status: string;
	granted_at: string;
	expires_at?: string | null;
};

type ResetCreditsPayload = { credits?: ResetCredit[] };
type ResetResult = {
	result?: string;
	status?: string;
	rate_limit_windows_reset?: number;
};

type CodexConnection = {
	native: boolean;
	headers: Record<string, string>;
	baseURL?: string;
	claims?: Claims;
};

export default function (pi: ExtensionAPI) {
	let timer: ReturnType<typeof setInterval> | undefined;
	let generation = 0;
	let activeContext: ExtensionContext | undefined;

	const clear = (ctx: ExtensionContext) =>
		ctx.ui.setStatus(STATUS_KEY, undefined);
	const selectedModelKey = (ctx: ExtensionContext) =>
		ctx.model ? `${ctx.model.provider}/${ctx.model.id}` : undefined;

	const refresh = async (
		ctx: ExtensionContext,
		reportError = false,
	): Promise<Status | undefined> => {
		activeContext = ctx;
		const modelKey = selectedModelKey(ctx);
		const currentGeneration = ++generation;
		try {
			const status = await fetchStatus(ctx);
			if (currentGeneration !== generation || modelKey !== selectedModelKey(ctx))
				return undefined;
			ctx.ui.setStatus(
				STATUS_KEY,
				ctx.ui.theme.fg("dim", formatStatusLine(status)),
			);
			return status;
		} catch (error) {
			if (currentGeneration === generation) clear(ctx);
			if (reportError) {
				ctx.ui.notify(
					error instanceof Error ? error.message : String(error),
					"error",
				);
			}
			return undefined;
		}
	};

	pi.registerCommand("codex-usage", {
		description: "Show ChatGPT Codex account and rate-limit usage",
		handler: async (_args: string, ctx: ExtensionContext) => {
			const status = await refresh(ctx, true);
			if (status) ctx.ui.notify(formatStatusCard(status), "info");
		},
	});

	pi.registerCommand("codex-reset", {
		description: "List banked Codex resets or activate an exact reset ID",
		handler: async (args: string, ctx: ExtensionContext) => {
			try {
				const creditID = args.trim();
				if (!creditID) {
					const credits = await fetchResetCredits(ctx);
					ctx.ui.notify(formatResetCredits(credits), "info");
					return;
				}
				if (/\s/.test(creditID)) {
					throw new Error("Usage: /codex-reset [reset-id]");
				}
				const result = await activateReset(ctx, creditID);
				ctx.ui.notify(formatResetResult(creditID, result), "info");
				await refresh(ctx);
			} catch (error) {
				ctx.ui.notify(
					error instanceof Error ? error.message : String(error),
					"error",
				);
			}
		},
	});

	pi.on("session_start", async (_event: unknown, ctx: ExtensionContext) => {
		activeContext = ctx;
		await refresh(ctx);
		if (!timer) {
			timer = setInterval(() => {
				if (activeContext) void refresh(activeContext);
			}, REFRESH_MS);
		}
	});

	pi.on("model_select", async (_event: unknown, ctx: ExtensionContext) => {
		activeContext = ctx;
		generation++;
		clear(ctx);
		await refresh(ctx);
	});

	pi.on("agent_settled", async (_event: unknown, ctx: ExtensionContext) => {
		activeContext = ctx;
		await refresh(ctx);
	});

	pi.on("session_shutdown", () => {
		generation++;
		activeContext = undefined;
		if (timer) clearInterval(timer);
		timer = undefined;
	});
}

async function resolveCodexConnection(
	ctx: ExtensionContext,
): Promise<CodexConnection> {
	const model = ctx.model;
	if (!model) throw new Error("No model selected");
	const auth = await ctx.modelRegistry.getProviderAuth(model.provider);
	if (!auth?.auth.apiKey)
		throw new Error(`No authentication available for ${model.provider}`);
	const token = auth.auth.apiKey;

	if (model.provider === "openai-codex") {
		const claims = decodeJWT(token);
		const accountID = claims["https://api.openai.com/auth"]?.chatgpt_account_id;
		if (!accountID)
			throw new Error("Codex access token has no ChatGPT account ID");
		return {
			native: true,
			claims,
			headers: {
				Authorization: `Bearer ${token}`,
				"ChatGPT-Account-Id": accountID,
				originator: "pi",
			},
		};
	}

	const baseURL = auth.auth.baseUrl ?? model.baseUrl;
	if (!baseURL) throw new Error("Selected provider has no base URL");
	return {
		native: false,
		baseURL: baseURL.replace(/\/$/, ""),
		headers: {
			...(auth.auth.headers ?? {}),
			Authorization: `Bearer ${token}`,
		},
	};
}

async function fetchStatus(ctx: ExtensionContext): Promise<Status> {
	const connection = await resolveCodexConnection(ctx);
	if (connection.native) {
		const payload = await getJSON(USAGE_URL, connection.headers);
		return parseStatus(payload, {
			email: connection.claims?.email,
			plan: connection.claims?.["https://api.openai.com/auth"]?.chatgpt_plan_type,
		});
	}

	const payload = await getJSON(
		`${connection.baseURL}/codex/usage`,
		connection.headers,
	);
	return parseStatus(payload);
}

async function fetchResetCredits(
	ctx: ExtensionContext,
): Promise<ResetCredit[]> {
	const connection = await resolveCodexConnection(ctx);
	const url = connection.native
		? RESET_CREDITS_URL
		: `${connection.baseURL}/codex/resets`;
	const payload = await requestJSON<ResetCreditsPayload>(url, {
		headers: connection.headers,
	});
	return [...(payload.credits ?? [])].sort((left, right) => {
		if (!left.expires_at) return right.expires_at ? 1 : 0;
		if (!right.expires_at) return -1;
		return Date.parse(left.expires_at) - Date.parse(right.expires_at);
	});
}

async function activateReset(
	ctx: ExtensionContext,
	creditID: string,
): Promise<ResetResult> {
	const connection = await resolveCodexConnection(ctx);
	const nativeBody = {
		redeem_request_id: crypto.randomUUID(),
		credit_id: creditID,
	};
	const url = connection.native
		? `${RESET_CREDITS_URL}/consume`
		: `${connection.baseURL}/codex/reset`;
	const payload = await requestJSON<ResetResult>(url, {
		method: "POST",
		headers: { ...connection.headers, "Content-Type": "application/json" },
		body: JSON.stringify(
			connection.native ? nativeBody : { credit_id: creditID },
		),
	});
	const result = payload.result || payload.status;
	if (
		!result ||
		!["reset", "already_redeemed", "nothing_to_reset", "no_credit"].includes(
			result,
		)
	) {
		throw new Error(
			`Activate Codex reset returned unexpected result: ${result || "missing"}`,
		);
	}
	return { ...payload, result };
}

async function getJSON(
	url: string,
	headers: Record<string, string>,
): Promise<UsagePayload> {
	return requestJSON<UsagePayload>(url, { headers });
}

async function requestJSON<T>(url: string, init: RequestInit): Promise<T> {
	const response = await fetch(url, {
		...init,
		signal: AbortSignal.timeout(15_000),
	});
	if (!response.ok) {
		const body = (await response.text()).trim();
		throw new Error(
			`Codex request failed (HTTP ${response.status})${body ? `: ${body}` : ""}`,
		);
	}
	return (await response.json()) as T;
}

function parseStatus(payload: UsagePayload, account = payload.account): Status {
	const primary = validWindow(payload.rate_limit?.primary_window);
	const secondary = validWindow(payload.rate_limit?.secondary_window);
	const weekly = [primary, secondary].find(
		(window) =>
			window &&
			Math.abs(window.limit_window_seconds - WEEK_SECONDS) <= WEEK_SECONDS * 0.05,
	);
	if (!primary && !weekly)
		throw new Error("ChatGPT did not return any Codex usage windows");
	return { account, primary, weekly };
}

function validWindow(value: Window | undefined): Window | undefined {
	return value &&
		Number.isFinite(value.used_percent) &&
		Number.isFinite(value.limit_window_seconds) &&
		Number.isFinite(value.reset_at)
		? value
		: undefined;
}

function formatStatusLine(status: Status): string {
	return [
		status.primary &&
			compactWindow(status.primary, durationLabel(status.primary)),
		status.weekly &&
			status.weekly !== status.primary &&
			compactWindow(status.weekly, "w"),
	]
		.filter(Boolean)
		.join("  ");
}

function compactWindow(window: Window, label: string): string {
	const left = percentLeft(window);
	return `${formatPercent(left)}/${label} (resets ${formatReset(window, true)})`;
}

function formatStatusCard(status: Status): string {
	const lines = [
		"ChatGPT Codex status",
		"Visit https://chatgpt.com/codex/settings/usage for up-to-date information on rate limits and credits.",
	];
	if (status.account?.email) {
		const plan = status.account.plan
			? ` (${titleWord(status.account.plan)})`
			: "";
		lines.push(`Account:       ${status.account.email}${plan}`);
	}
	if (status.primary)
		lines.push(
			`${durationLabel(status.primary)} limit:      ${fullWindow(status.primary)}`,
		);
	if (status.weekly && status.weekly !== status.primary)
		lines.push(`Weekly limit: ${fullWindow(status.weekly)}`);
	return lines.join("\n");
}

function fullWindow(window: Window): string {
	const left = percentLeft(window);
	const filled = Math.max(0, Math.min(20, Math.round((left * 20) / 100)));
	return `[${"█".repeat(filled)}${"░".repeat(20 - filled)}] ${formatPercent(left)} left (resets ${formatReset(window, true)})`;
}

function percentLeft(window: Window): number {
	return 100 - Math.max(0, Math.min(100, window.used_percent));
}

function durationLabel(window: Window): string {
	const hours = window.limit_window_seconds / 3600;
	return Number.isInteger(hours) ? `${hours}h` : "rate";
}

function formatPercent(value: number): string {
	return `${Number.isInteger(value) ? value : value.toFixed(1)}%`;
}

function formatReset(window: Window, includeWeeklyDate: boolean): string {
	const date = new Date(window.reset_at * 1000);
	const time = date.toLocaleTimeString([], {
		hour: "2-digit",
		minute: "2-digit",
		hour12: false,
	});
	if (!includeWeeklyDate || window.limit_window_seconds < 24 * 60 * 60)
		return time;
	const day = date.toLocaleDateString([], { day: "numeric", month: "short" });
	return `${time} on ${day}`;
}

function decodeJWT(token: string): Claims {
	const encoded = token.split(".")[1];
	if (!encoded) throw new Error("Invalid Codex access token");
	try {
		const normalized = encoded.replace(/-/g, "+").replace(/_/g, "/");
		const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, "=");
		const bytes = Uint8Array.from(atob(padded), (character) =>
			character.charCodeAt(0),
		);
		return JSON.parse(new TextDecoder().decode(bytes)) as Claims;
	} catch (error) {
		throw new Error("Could not decode Codex access token", { cause: error });
	}
}

function formatResetCredits(credits: ResetCredit[]): string {
	if (credits.length === 0) return "No banked rate-limit reset credits.";
	const lines = ["Banked Codex rate-limit resets:"];
	for (const credit of credits) {
		const granted = formatCreditTime(credit.granted_at);
		const expires = credit.expires_at
			? formatCreditTime(credit.expires_at)
			: "never/unknown";
		lines.push(
			`${credit.id}  ${credit.status}  granted ${granted}  expires ${expires}`,
		);
	}
	lines.push("Activate one with /codex-reset <reset-id>.");
	return lines.join("\n");
}

function formatResetResult(creditID: string, payload: ResetResult): string {
	const result = payload.result || payload.status || "unknown";
	const windows = payload.rate_limit_windows_reset ?? 0;
	return result === "reset"
		? `Reset ${creditID} activated (${windows} rate-limit windows reset).`
		: `Reset ${creditID} result: ${result} (${windows} rate-limit windows reset).`;
}

function formatCreditTime(value: string): string {
	const date = new Date(value);
	if (!Number.isFinite(date.getTime())) return value;
	return `${date.toISOString().slice(0, 16).replace("T", " ")} UTC`;
}

function titleWord(value: string): string {
	return value.charAt(0).toUpperCase() + value.slice(1).toLowerCase();
}
