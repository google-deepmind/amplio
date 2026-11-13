/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { serverSettings } from './serverSettings.svelte';

// Absolute times render in the server's time zone, the one the agents see
// (serverSettings.displayZone; the browser's until it is known). Formatters are
// cached: chat logs format hundreds of timestamps.
const formatters = new Map<string, Intl.DateTimeFormat>();
function formatter(locale: string | undefined, opts: Intl.DateTimeFormatOptions): Intl.DateTimeFormat {
	const timeZone = serverSettings.displayZone;
	const key = JSON.stringify([locale, timeZone, opts]);
	let f = formatters.get(key);
	if (!f) {
		f = new Intl.DateTimeFormat(locale, { ...opts, timeZone });
		formatters.set(key, f);
	}
	return f;
}

const NUMERIC: Intl.DateTimeFormatOptions = {
	year: 'numeric',
	month: '2-digit',
	day: '2-digit',
	hour: '2-digit',
	minute: '2-digit',
	second: '2-digit',
	hourCycle: 'h23'
};

// numericParts are d's date and time fields (and with zoneName, the zone's
// short name) in the server's zone.
function numericParts(d: Date, zoneName = false): Record<string, string> {
	const f = formatter('en-US', zoneName ? { ...NUMERIC, timeZoneName: 'short' } : NUMERIC);
	const out: Record<string, string> = {};
	for (const p of f.formatToParts(d)) out[p.type] = p.value;
	return out;
}

function toDate(t: string | number): Date | null {
	const d = new Date(t);
	return isNaN(d.getTime()) ? null : d;
}

// ISO-ish absolute format (YYYY-MM-DD HH:MM:SS). Used in metadata cards and
// tooltips where users want a sortable, unambiguous timestamp without locale
// guesswork.
export function formatLocalIso(iso: string): string {
	const d = toDate(iso);
	if (!d) return '';
	const p = numericParts(d);
	return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}`;
}

// The same with the zone's short name, e.g. "2026-06-05 22:24:44 PDT".
export function formatTimestamp(t: string | number): string {
	const d = toDate(t);
	if (!d) return '';
	const p = numericParts(d, true);
	const tz = p.timeZoneName ? ' ' + p.timeZoneName : '';
	return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}${tz}`;
}

// To the minute (YYYY-MM-DD HH:MM).
export function formatMinute(iso: string): string {
	const d = toDate(iso);
	if (!d) return '';
	const p = numericParts(d);
	return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}`;
}

// The browser's locale format, as Date.toLocaleString() and
// toLocaleDateString() write it.
export function formatLocale(t: string | number): string {
	const d = toDate(t);
	if (!d) return '';
	return formatter(undefined, {
		year: 'numeric',
		month: 'numeric',
		day: 'numeric',
		hour: 'numeric',
		minute: 'numeric',
		second: 'numeric'
	}).format(d);
}

export function formatLocaleDate(t: string | number): string {
	const d = toDate(t);
	if (!d) return '';
	return formatter(undefined, { year: 'numeric', month: 'numeric', day: 'numeric' }).format(d);
}

// Compact relative time for list rows ("just now", "3m", "5h", "2d", then a
// locale date; with `compact`, "3w" / "5mo" / "2y" instead). Shared by the run
// card and the session rows.
export function timeAgo(iso: string, compact = false): string {
	const t = new Date(iso).getTime();
	if (!t) return '';
	const s = Math.floor((Date.now() - t) / 1000);
	if (s < 60) return 'just now';
	const m = Math.floor(s / 60);
	if (m < 60) return `${m}m`;
	const h = Math.floor(m / 60);
	if (h < 24) return `${h}h`;
	const d = Math.floor(h / 24);
	if (d < 7) return `${d}d`;
	if (!compact) return formatLocaleDate(t);
	if (d < 30) return `${Math.floor(d / 7)}w`;
	if (d < 365) return `${Math.floor(d / 30)}mo`;
	return `${Math.floor(d / 365)}y`;
}

// Compact elapsed duration since a timestamp, for live "running for…" tickers
// (the chat's working indicator, the overview's report-generation banner). Caller
// passes `nowMs` (usually a 1s-ticking $state) so the value updates reactively.
// Format: "Ns" under a minute, "Nm Ns" under an hour, "Nh Nm" beyond.
export function formatElapsed(sinceIso: string, nowMs: number): string {
	const t = new Date(sinceIso).getTime();
	if (!t) return '';
	const s = Math.max(0, Math.floor((nowMs - t) / 1000));
	if (s < 60) return `${s}s`;
	const m = Math.floor(s / 60);
	if (m < 60) return `${m}m ${s % 60}s`;
	return `${Math.floor(m / 60)}h ${m % 60}m`;
}
