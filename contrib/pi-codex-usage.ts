// @ts-expect-error Pi provides this module when it loads the extension.
import type {
	ExtensionAPI,
	ExtensionContext,
} from "@earendil-works/pi-coding-agent";

const STATUS_KEY = "codex-usage";
const USAGE_URL = "https://chatgpt.com/backend-api/wham/usage";
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

async function fetchStatus(ctx: ExtensionContext): Promise<Status> {
	const model = ctx.model;
	if (!model) throw new Error("No model selected");
	const auth = await ctx.modelRegistry.getProviderAuth(model.provider);
	if (!auth?.auth.apiKey)
		throw new Error(`No authentication available for ${model.provider}`);

	// Only the built-in provider talks directly to ChatGPT. Other providers,
	// including codex-gateway, must be queried through their own base URL even
	// when they happen to use the Codex Responses protocol.
	if (model.provider === "openai-codex") {
		const claims = decodeJWT(auth.auth.apiKey);
		const accountID = claims["https://api.openai.com/auth"]?.chatgpt_account_id;
		if (!accountID)
			throw new Error("Codex access token has no ChatGPT account ID");
		const payload = await getJSON(USAGE_URL, {
			Authorization: `Bearer ${auth.auth.apiKey}`,
			"ChatGPT-Account-Id": accountID,
			originator: "pi",
		});
		return parseStatus(payload, {
			email: claims.email,
			plan: claims["https://api.openai.com/auth"]?.chatgpt_plan_type,
		});
	}

	const baseURL = auth.auth.baseUrl ?? model.baseUrl;
	if (!baseURL) throw new Error("Selected provider has no base URL");
	const payload = await getJSON(`${baseURL.replace(/\/$/, "")}/codex/usage`, {
		...(auth.auth.headers ?? {}),
		Authorization: `Bearer ${auth.auth.apiKey}`,
	});
	return parseStatus(payload);
}

async function getJSON(
	url: string,
	headers: Record<string, string>,
): Promise<UsagePayload> {
	const response = await fetch(url, {
		headers,
		signal: AbortSignal.timeout(15_000),
	});
	if (!response.ok) {
		const body = (await response.text()).trim();
		throw new Error(
			`Codex usage request failed (HTTP ${response.status})${body ? `: ${body}` : ""}`,
		);
	}
	return (await response.json()) as UsagePayload;
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

function titleWord(value: string): string {
	return value.charAt(0).toUpperCase() + value.slice(1).toLowerCase();
}
