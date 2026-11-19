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

// The loop form's fields and the iCalendar rules they stand for: a plain
// interval anchored in UTC (DTSTART ...Z) with an optional COUNT or UNTIL, as
// internal/loop's Form builds them. Rules of any other shape are the server's
// to describe (LoopDTO.summary). Pure, so `npm test` can exercise it in node.

export type Unit = 'minutes' | 'hours' | 'days';

export const UNIT_SECONDS: Record<Unit, number> = { minutes: 60, hours: 3600, days: 86400 };

export type Ends =
	| { kind: 'after'; n: number | null; unit: Unit } // a duration from now
	| { kind: 'times'; n: number | null } // ticks; with send now, the first counts
	| { kind: 'never' }
	| { kind: 'until'; at: number }; // an existing loop's end (ms), kept while editing

export interface LoopFields {
	every: number | null; // required; null until the operator fills it in
	unit: Unit;
	sendNow: boolean;
	ends: Ends;
}

// A rule of the shape the form writes.
export interface PlainRule {
	start: number; // DTSTART, ms
	seconds: number; // the interval
	count?: number;
	until?: number; // ms
}

const FREQ_SECONDS: Record<string, number> = { MINUTELY: 60, HOURLY: 3600, DAILY: 86400 };

// The largest unit that divides `seconds`, and how many of it.
export function inUnits(seconds: number): { n: number; unit: Unit } {
	for (const unit of ['days', 'hours'] as const) {
		if (seconds % UNIT_SECONDS[unit] === 0) return { n: seconds / UNIT_SECONDS[unit], unit };
	}
	return { n: Math.ceil(seconds / 60), unit: 'minutes' };
}

export function intervalSeconds(f: LoopFields): number {
	return (f.every ?? 0) * UNIT_SECONDS[f.unit];
}

function icalUTC(ms: number): string {
	return new Date(ms).toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');
}

function parseICalUTC(s: string): number | null {
	const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/.exec(s);
	if (!m) return null;
	const [y, mo, d, h, mi, se] = m.slice(1).map(Number);
	return Date.UTC(y, mo - 1, d, h, mi, se);
}

const floorSecond = (ms: number) => Math.floor(ms / 1000) * 1000;

// An existing loop's schedule, which an edit of it starts from (see editFrom).
export interface EditFrom {
	seconds: number; // its interval
	next: number | null; // its next occurrence on its rule's grid (ms); null if none is left
	last: number | null; // its last tick (ms); null if it hasn't ticked
	created: number; // when it was set (ms)
}

// When the first tick of a rule built at `now` is due. A new loop starts now
// (send now) or one interval later. An edit keeps the loop's next occurrence
// while the interval stays (only the end changed), and its start while it
// hasn't ticked; a new interval on a loop that has ticked counts from its last
// tick. Never before now: the server treats an edited rule as starting over,
// and counts occurrences before now as spent.
export function firstTick(f: LoopFields, now: number, edit?: EditFrom): number {
	const step = intervalSeconds(f) * 1000;
	const t0 = floorSecond(now);
	if (!edit) return f.sendNow ? t0 : t0 + step;
	if (edit.next !== null && (edit.last === null || intervalSeconds(f) === edit.seconds)) return Math.max(edit.next, t0);
	return Math.max(floorSecond(edit.last ?? edit.created) + step, t0);
}

// The end a rule built at `now` would have, in ms, if it ends at a time; null
// also for a duration validate refuses (one that long isn't a valid date).
export function endsAt(f: LoopFields, now: number): number | null {
	if (f.ends.kind === 'until') return f.ends.at;
	if (f.ends.kind !== 'after' || !isWhole(f.ends.n)) return null;
	const seconds = f.ends.n * UNIT_SECONDS[f.ends.unit];
	return seconds > MAX_END_SECONDS ? null : floorSecond(now) + seconds * 1000;
}

const isWhole = (n: number | null): n is number => n !== null && Number.isSafeInteger(n) && n >= 1;

// The server's limits on a rule: INTERVAL (internal/loop's maxInterval), and
// sane ends for the form.
export const MAX_INTERVAL = 100_000;
export const MAX_TICKS = 1_000_000;
const MAX_END_SECONDS = 10 * 366 * 86400;

// The frequency the rule for an interval uses: the largest unit dividing it.
function frequency(seconds: number): { freq: string; unit: number; name: string } {
	if (seconds % 86400 === 0) return { freq: 'DAILY', unit: 86400, name: 'days' };
	if (seconds % 3600 === 0) return { freq: 'HOURLY', unit: 3600, name: 'hours' };
	return { freq: 'MINUTELY', unit: 60, name: 'minutes' };
}

// What's wrong with the fields, or null if they make a rule.
export function validate(f: LoopFields, minIntervalSeconds: number, now: number, edit?: EditFrom): string | null {
	if (f.every === null) return 'Set how often it repeats.';
	if (!isWhole(f.every)) return 'The interval is a whole number of minutes, hours or days.';
	if (intervalSeconds(f) < minIntervalSeconds) {
		return `Loops repeat at most every ${phrase(minIntervalSeconds)}.`;
	}
	const { unit, name } = frequency(intervalSeconds(f));
	if (intervalSeconds(f) / unit > MAX_INTERVAL) return `An interval goes up to ${MAX_INTERVAL} ${name}.`;
	if ((f.ends.kind === 'after' || f.ends.kind === 'times') && !isWhole(f.ends.n)) {
		return f.ends.kind === 'after' ? 'Set how long it runs.' : 'Set how many times it runs.';
	}
	if (f.ends.kind === 'after' && (f.ends.n as number) * UNIT_SECONDS[f.ends.unit] > MAX_END_SECONDS) {
		return 'A loop can run for up to 10 years.';
	}
	if (f.ends.kind === 'times' && (f.ends.n as number) > MAX_TICKS) return `A loop can run up to ${MAX_TICKS} times.`;
	const end = endsAt(f, now);
	if (end !== null && end < firstTick(f, now, edit)) return 'It would end before its first tick.';
	return null;
}

// The iCalendar spec for the fields at `now` (see firstTick for edit). The
// caller validates first.
export function buildSpec(f: LoopFields, now: number, edit?: EditFrom): string {
	const seconds = intervalSeconds(f);
	const { freq, unit } = frequency(seconds);
	let rule = `RRULE:FREQ=${freq};INTERVAL=${seconds / unit}`;
	const end = endsAt(f, now);
	if (f.ends.kind === 'times') rule += `;COUNT=${f.ends.n}`;
	else if (end !== null) rule += `;UNTIL=${icalUTC(end)}`;
	return `DTSTART:${icalUTC(firstTick(f, now, edit))}\n${rule}`;
}

// parseSpec reads a rule of the form's shape, or returns null for any other
// (a floating or TZID start, BY* parts, RDATEs, other frequencies).
export function parseSpec(spec: string): PlainRule | null {
	const lines = spec.trim().split(/\s+/);
	if (lines.length !== 2 || !lines[0].startsWith('DTSTART:') || !lines[1].startsWith('RRULE:')) return null;
	const start = parseICalUTC(lines[0].slice('DTSTART:'.length));
	if (start === null) return null;
	const parts = new Map<string, string>();
	for (const p of lines[1].slice('RRULE:'.length).split(';')) {
		const [k, v, extra] = p.split('=');
		if (!k || v === undefined || extra !== undefined || parts.has(k)) return null;
		parts.set(k, v);
	}
	const unit = FREQ_SECONDS[parts.get('FREQ') ?? ''];
	const interval = Number(parts.get('INTERVAL') ?? '1');
	if (!unit || !isWhole(interval)) return null;
	const rule: PlainRule = { start, seconds: interval * unit };
	for (const [k, v] of parts) {
		if (k === 'COUNT' && isWhole(Number(v))) rule.count = Number(v);
		else if (k === 'UNTIL' && parseICalUTC(v) !== null) rule.until = parseICalUTC(v) as number;
		else if (k !== 'FREQ' && k !== 'INTERVAL') return null;
	}
	if (rule.count !== undefined && rule.until !== undefined) return null;
	return rule;
}

// "minute", "30 minutes", "hour", "2 days": an interval in its largest unit.
export function phrase(seconds: number): string {
	const { n, unit } = inUnits(seconds);
	const one = unit.slice(0, -1);
	return n === 1 ? one : `${n} ${unit}`;
}

// "30m", "2h", "1d": the same, for a chip.
export function shortPhrase(seconds: number): string {
	const { n, unit } = inUnits(seconds);
	return `${n}${unit[0]}`;
}

// A date's fields in `timeZone` (the server's; undefined = the browser's).
function zoneParts(ms: number, timeZone: string | undefined): Record<string, string> {
	const f = new Intl.DateTimeFormat('en-US', {
		timeZone,
		weekday: 'short',
		month: 'short',
		day: 'numeric',
		year: 'numeric',
		hour: '2-digit',
		minute: '2-digit',
		hourCycle: 'h23',
		timeZoneName: 'short'
	});
	return Object.fromEntries(f.formatToParts(new Date(ms)).map((p) => [p.type, p.value]));
}

// "Tue Nov 17 15:30" in `timeZone`, with the year when it isn't `now`'s, and
// with `zone` the zone's short name ("PST").
export function formatWhen(ms: number, timeZone: string | undefined, now: number, opts: { zone?: boolean } = {}): string {
	const p = zoneParts(ms, timeZone);
	const year = p.year !== zoneParts(now, timeZone).year ? ` ${p.year}` : '';
	const zone = opts.zone ? ` ${p.timeZoneName}` : '';
	return `${p.weekday} ${p.month} ${p.day}${year} ${p.hour}:${p.minute}${zone}`;
}

// The same, short: "15:30" on `now`'s day, else "Nov 17" (with the year when
// it isn't `now`'s).
export function formatShort(ms: number, timeZone: string | undefined, now: number): string {
	const p = zoneParts(ms, timeZone);
	const n = zoneParts(now, timeZone);
	if (p.year === n.year && p.month === n.month && p.day === n.day) return `${p.hour}:${p.minute}`;
	return `${p.month} ${p.day}${p.year !== n.year ? ` ${p.year}` : ''}`;
}

// When a tick comes: "15:30" on `now`'s day, else with the day ("Thu Nov 12 16:00").
export function formatNext(ms: number, timeZone: string | undefined, now: number): string {
	const short = formatShort(ms, timeZone, now);
	return short.includes(':') ? short : formatWhen(ms, timeZone, now);
}

// The day `ms` falls on in `timeZone`, as days since the epoch.
function zoneDay(ms: number, timeZone: string | undefined): number {
	const f = new Intl.DateTimeFormat('en-US', { timeZone, year: 'numeric', month: 'numeric', day: 'numeric' });
	const p = Object.fromEntries(f.formatToParts(ms).map((x) => [x.type, x.value]));
	return Date.UTC(Number(p.year), Number(p.month) - 1, Number(p.day)) / 86_400_000;
}

// When a tick comes, in as few characters as stay unambiguous: "15:30" on
// `now`'s day, "Tue 12:55" within the next 6 days, else "Nov 30" (with the
// year when it isn't `now`'s).
export function formatAhead(ms: number, timeZone: string | undefined, now: number): string {
	const days = zoneDay(ms, timeZone) - zoneDay(now, timeZone);
	if (days < 1 || days > 6) return formatShort(ms, timeZone, now);
	const p = zoneParts(ms, timeZone);
	return `${p.weekday} ${p.hour}:${p.minute}`;
}

// The summary line for a rule of the form's shape, as a calendar shows a
// recurrence: "Every 30 minutes, until Tue Nov 17 15:30", "Every hour, 5 times".
export function describeRule(r: PlainRule, timeZone: string | undefined, now: number): string {
	const every = `Every ${phrase(r.seconds)}`;
	if (r.count !== undefined) return `${every}, ${r.count === 1 ? 'once' : `${r.count} times`}`;
	if (r.until !== undefined) return `${every}, until ${formatWhen(r.until, timeZone, now)}`;
	return every;
}

// The summary for any spec: ours, or the server's for a rule the form doesn't
// write.
export function describeSpec(spec: string, serverSummary: string, timeZone: string | undefined, now: number): string {
	const r = parseSpec(spec);
	return r ? describeRule(r, timeZone, now) : serverSummary;
}

// The rule's occurrences before its next tick: spent. Rounded down: a tick
// held past its grid point (the scheduler's minute between ticks) still counts
// the occurrence it's for, not the next.
function spentBefore(r: PlainRule, nextTick: number | null): number {
	return nextTick === null ? 0 : Math.max(0, Math.floor((nextTick - r.start) / (r.seconds * 1000)));
}

// The fields that rebuild a loop's rule from its next tick on, for editing
// it. The end is kept: an UNTIL as that date, a COUNT as the ticks left
// (occurrences from DTSTART up to the next tick are spent). Null for a rule
// the form can't show.
export function fieldsForEdit(spec: string, nextTick: number | null): LoopFields | null {
	const r = parseSpec(spec);
	if (!r) return null;
	const { n, unit } = inUnits(r.seconds);
	let ends: Ends = { kind: 'never' };
	if (r.until !== undefined) ends = { kind: 'until', at: r.until };
	else if (r.count !== undefined) ends = { kind: 'times', n: Math.max(1, r.count - spentBefore(r, nextTick)) };
	return { every: n, unit, sendNow: false, ends };
}

// Where an edit of a loop (its DTO as the server lists it) starts from, or
// undefined for a rule the form can't show. Its next occurrence is the grid
// point of its next tick, so a kept interval keeps the rule's grid and the
// COUNT fieldsForEdit leaves.
export function editFrom(loop: {
	schedule: { spec: string };
	created_at: string;
	last_tick?: string;
	next_tick?: string;
}): EditFrom | undefined {
	const r = parseSpec(loop.schedule.spec);
	if (!r) return undefined;
	const nextTick = loop.next_tick ? Date.parse(loop.next_tick) : null;
	return {
		seconds: r.seconds,
		next: nextTick === null ? null : r.start + spentBefore(r, nextTick) * r.seconds * 1000,
		last: loop.last_tick ? Date.parse(loop.last_tick) : null,
		created: Date.parse(loop.created_at)
	};
}

export function sameFields(a: LoopFields, b: LoopFields): boolean {
	return JSON.stringify(a) === JSON.stringify(b);
}

// [loop] expiry from GET /api/settings (a Go duration such as "168h", or
// "never") as the form's default end.
export function defaultEnds(expiry: string | undefined): Ends {
	if (expiry === 'never') return { kind: 'never' };
	const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(expiry ?? '');
	const seconds = m && expiry ? Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Number(m[3] ?? 0) : 0;
	if (seconds <= 0) return { kind: 'after', n: 7, unit: 'days' };
	return { kind: 'after', ...inUnits(seconds) };
}

// A tick's label: "loop 1 · tick 2", and "· verification v3" for the version
// it started (a loop that verifies each tick).
export function tickLabel(t: { id: string; n: number; version?: number }): string {
	return `loop ${t.id} · tick ${t.n}${t.version ? ` · verification v${t.version}` : ''}`;
}

// A tick's content is "[loop <id> · <server summary> · tick <n>] <prompt>",
// with " · verification v<k>" before the "]" for the version it started;
// splitTick takes the header apart, or returns null if it isn't one.
export function splitTick(content: string, id: string, n: number): { summary: string; prompt: string } | null {
	const head = `[loop ${id} · `;
	if (!content.startsWith(head)) return null;
	const tail = new RegExp(` · tick ${n}(?: · verification v\\d+)?\\] `).exec(content.slice(head.length));
	if (!tail) return null;
	const end = head.length + tail.index;
	return { summary: content.slice(head.length, end), prompt: content.slice(end + tail[0].length) };
}

// The server's clock minus the browser's, from a reading of the server's clock
// (RFC 3339) taken between sent and received (browser ms): 0 if unreadable. A
// rule's "now" is the server's, which the scheduler compares it with.
export function clockOffset(serverNow: string | undefined, sent: number, received: number): number {
	const t = serverNow ? Date.parse(serverNow) : NaN;
	return Number.isFinite(t) ? Math.round(t - (sent + received) / 2) : 0;
}

// What an edit started from, taken when Edit was clicked: the form's fields
// (null for a rule the form can't show), and the loop's ticks and prompt.
export interface EditOpened {
	fields: LoopFields | null;
	ticks: number;
	prompt: string;
}

// The PATCH body for an edit of `loop` (as it is now): only what the operator
// changed since the form opened, so a prompt changed elsewhere meanwhile isn't
// sent back; the prompt trimmed. A changed schedule is rebuilt from the loop's
// schedule as it is now (see editFrom and firstTick). An "N more ticks" end
// counts from when the form opened: ticks sent since are taken off, and if
// none are left the edit is refused rather than sending more than the
// operator set.
export function loopPatch(
	loop: Parameters<typeof editFrom>[0] & { id: string; ticks: number },
	opened: EditOpened,
	after: LoopFields | null,
	prompt: string,
	now: number
): { prompt?: string; schedule?: { kind: string; spec: string } } {
	const patch: { prompt?: string; schedule?: { kind: string; spec: string } } = {};
	if (prompt.trim() !== opened.prompt) patch.prompt = prompt.trim();
	if (after && !(opened.fields && sameFields(opened.fields, after))) {
		let fields = after;
		const since = loop.ticks - opened.ticks;
		if (since > 0 && after.ends.kind === 'times') {
			const n = (after.ends.n ?? 0) - since;
			if (n < 1) {
				throw new Error(`Loop ${loop.id} ticked while you edited it, using up the ticks you set; edit it again.`);
			}
			fields = { ...after, ends: { kind: 'times', n } };
		}
		const from = editFrom(loop);
		if (!from) throw new Error(`Loop ${loop.id}'s rule was changed meanwhile to one this form doesn't write; edit it again.`);
		patch.schedule = { kind: 'rrule', spec: buildSpec(fields, now, from) };
	}
	return patch;
}
