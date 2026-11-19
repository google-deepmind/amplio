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

// What a composer attaches to the message being sent: the
// verification form's values, the chips' labels, and the slash shortcuts.
// Pure, so `npm test` can exercise it in node.

import { endsAt, formatShort, intervalSeconds, shortPhrase, type LoopFields } from './loopSchedule.ts';
import type { LoopVerify } from './types.ts';

// The verification form's fields. A blank one is left to the server, which
// fills it from its [verify] defaults when it stores the request.
export interface VerifyFields {
	criteria: string; // "" = the agent's current request
	model: string; // "" = the server's [verify] model, else the agent's own
	max_rounds: number | null; // null = the server's [verify] max_rounds
}

// A new verification as the form first shows it: all blank, so the server's
// defaults apply when it is stored, not the ones this page knew when the form
// opened.
export function newVerifyFields(): VerifyFields {
	return { criteria: '', model: '', max_rounds: null };
}

// The body of PUT .../verify and of `verify` on a message or a new run.
export interface VerifyRequest {
	criteria?: string;
	model?: string;
	max_rounds?: number;
}

export function verifyRequest(f: VerifyFields): VerifyRequest {
	const r: VerifyRequest = {};
	if (f.criteria.trim()) r.criteria = f.criteria.trim();
	if (f.model) r.model = f.model;
	if (f.max_rounds !== null) r.max_rounds = f.max_rounds;
	return r;
}

export function validateVerify(f: VerifyFields): string | null {
	if (f.max_rounds !== null && !(Number.isInteger(f.max_rounds) && f.max_rounds >= 0)) {
		return 'Max rounds is a whole number (0 = unbounded).';
	}
	return null;
}

export function roundsText(n: number): string {
	if (n === 0) return 'unbounded';
	return n === 1 ? '1 round' : `${n} rounds`;
}

// "Verify · 3 rounds"; with a repeat, "Verify each tick · 3 rounds".
export function verifyChip(f: VerifyFields, defaultRounds: number, eachTick = false): string {
	return `${eachTick ? 'Verify each tick' : 'Verify'} · ${roundsText(f.max_rounds ?? defaultRounds)}`;
}

// "Repeat every 30m · until Nov 17", "Repeat every 1h · 5 times": the end in
// the server's zone, by its time alone when that's today. Without send now,
// "· first in 30m"; for a loop that verifies its ticks, "· verify each tick".
export function loopChip(f: LoopFields, timeZone: string | undefined, now: number, eachTick = false): string {
	const every = shortPhrase(intervalSeconds(f));
	const head = `Repeat every ${every}`;
	const tail = (f.sendNow ? '' : ` · first in ${every}`) + (eachTick ? ' · verify each tick' : '');
	if (f.ends.kind === 'times') return `${head} · ${f.ends.n === 1 ? 'once' : `${f.ends.n} times`}${tail}`;
	if (f.ends.kind === 'never') return `${head} · no end${tail}`;
	const end = endsAt(f, now);
	return (end === null ? head : `${head} · until ${formatShort(end, timeZone, now)}`) + tail;
}

// What verifying each tick means: every tick starts a new verification, which
// replaces the session's current one (it has one at a time) and can take up to
// max rounds verifier runs.
export function tickCostNote(rounds: number): string {
	const head = "Each tick starts a new verification, replacing the session's current one";
	if (rounds === 0) return `${head}, with no limit on verifier runs per tick.`;
	return `${head}: up to ${rounds} verifier ${rounds === 1 ? 'run' : 'runs'} per tick.`;
}

// The new-run form with both the task's Verify and the loop's Verify each tick.
export const TASK_SUPERSEDED = "If the task hasn't concluded by the loop's first tick, that tick's verification replaces the task's.";

// The warning that goes with a composer's chips, or "": what verifying each
// tick costs (not while the Verify form, which says so, is open), or, on the
// new-run form, that the loop's verification supersedes the task's.
export function optionsNote(
	s: {
		verify: VerifyFields | null;
		loop: LoopFields | null;
		loopVerify: VerifyFields | null;
		open: Shortcut | null;
		ownLoopVerify: boolean;
	},
	defaultRounds: number
): string {
	if (!s.verify || !s.loop) return '';
	if (!s.ownLoopVerify) return s.open === 'verify' ? '' : tickCostNote(s.verify.max_rounds ?? defaultRounds);
	return s.loopVerify && !s.open ? TASK_SUPERSEDED : '';
}

// A loop's verification (as GET .../loops shows it) as the form's fields.
export function loopVerifyFields(v: LoopVerify | undefined): VerifyFields | null {
	return v ? { criteria: v.criteria ?? '', model: v.model ?? '', max_rounds: v.max_rounds } : null;
}

// The `verify` of a loop's PATCH: the request the form now shows, null to
// stop verifying its ticks, or undefined if it didn't change.
export function verifyPatch(before: VerifyFields | null, after: VerifyFields | null): VerifyRequest | null | undefined {
	if (JSON.stringify(before) === JSON.stringify(after)) return undefined;
	return after ? verifyRequest(after) : null;
}

export type Shortcut = 'verify' | 'loop';

export const NOT_CONCLUDING =
	'Interactive sessions cannot be verified, as they never conclude. To manually add verification to sub-agents, use the Inspector tab.';

// Why an option can't be added to a message now, or "". Verify with Repeat is
// a loop that verifies each tick.
export function blockedOption(which: Shortcut, s: { offersVerify: boolean }): string {
	return which === 'verify' && !s.offersVerify ? NOT_CONCLUDING : '';
}

// A slash shortcut: `/verify` or `/loop` as the first word of a message opens
// that form and is removed; nothing after it is parsed. While typing, the word
// counts once whitespace follows it (so `/loops` isn't one); at send (`final`)
// it may also be the whole message.
export function slashShortcut(text: string, final = false): { cmd: Shortcut; rest: string } | null {
	const m = /^\/(verify|loop)(\s+|$)/.exec(text);
	if (!m || (!m[2] && !final)) return null;
	return { cmd: m[1] as Shortcut, rest: text.slice(m[0].length) };
}
