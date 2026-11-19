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

// How the inspector words a session's verification (GET .../verify): its state
// and each change in its history. Pure, so `npm test` can exercise it in node.

import type { LoopVerify, VerifyChange, VerifyOutcome, VerifyReport } from './types.ts';

// "round 2 of 3", or "round 2" when unbounded.
export function roundOf(round: number, maxRounds: number): string {
	return maxRounds > 0 ? `round ${round} of ${maxRounds}` : `round ${round}`;
}

// A candidate by its version and number: "v2, candidate 1", "v2" for a
// version alone, "" without a version (a server before versions).
export function labelOf(version?: number, candidate?: number): string {
	if (!version) return '';
	return candidate ? `v${version}, candidate ${candidate}` : `v${version}`;
}

// The state as a short label and a sentence.
export function verifyState(r: VerifyReport): { label: string; detail: string } {
	const v = r.version ? ` (v${r.version})` : '';
	switch (r.state) {
		case 'on':
			return {
				label: 'on',
				detail:
					r.round > 1
						? `Waiting for the next conclusion (${[labelOf(r.version), roundOf(r.round, r.max_rounds)].filter(Boolean).join(', ')}, after ${r.round - 1} ${r.round === 2 ? 'rejection' : 'rejections'}).`
						: `Its next conclusion goes to a verifier first${v}.`
			};
		case 'verifying':
			return {
				label: 'verifying',
				detail: `${r.verifier} is checking its conclusion (${[labelOf(r.version, r.candidate), roundOf(r.round, r.max_rounds)].filter(Boolean).join('; ')}).`
			};
		case 'passed':
			return { label: 'passed', detail: `Verified by ${r.verifier} (round ${r.round}).` };
		case 'not_verified': {
			// The record says why when it isn't the rounds (no verifier, stops).
			const why = [...r.history].reverse().find((c) => c.action === 'exhausted')?.reason;
			if (why) return { label: 'not verified', detail: `Not verified: ${why}` };
			if (r.round === 0) return { label: 'not verified', detail: 'It ended without a verdict; see the history.' };
			return {
				label: 'not verified',
				detail: `The rounds ran out: ${r.round} ${r.round === 1 ? 'rejection' : 'rejections'}.`
			};
		}
		default:
			return { label: 'off', detail: 'Its conclusions are delivered without a check.' };
	}
}

// Whether the form edits the current request (on, verifying) rather than
// starting a new one.
export function verifyActive(r: VerifyReport | null | undefined): boolean {
	return r?.state === 'on' || r?.state === 'verifying';
}

function by(c: VerifyChange): string {
	if (!c.by || c.by === 'harness') return '';
	return c.by === 'operator' ? ' by the operator' : ` by ${c.by}`;
}

// One history entry, led by what it is about: "v1 · turned on by the
// operator", "v1, candidate 1 · sent to quick-rabbit (round 1)", "v1,
// candidate 1 · rejected by quick-rabbit (round 1)".
export function changeText(c: VerifyChange): string {
	const lab = labelOf(c.version, c.candidate);
	return lab ? `${lab} · ${changeWords(c)}` : changeWords(c);
}

function changeWords(c: VerifyChange): string {
	const round = c.round ? ` (round ${c.round})` : '';
	switch (c.action) {
		case 'set':
			return `turned on${by(c)}`;
		case 'replace':
			return `replaced${by(c)}`;
		case 'clear':
			return `turned off${by(c)}`;
		case 'verifying':
			return `sent to ${c.verifier}${round}`;
		case 'rejected':
			return `rejected by ${c.verifier}${round}`;
		case 'passed':
			return `passed: verified by ${c.verifier}${round}`;
		case 'exhausted':
			return `not verified${round}`;
		case 'withdrawn':
			return 'withdrawn: never delivered';
		case 'stale':
			return c.outcome ? `${c.outcome.verifier}'s ${verdictLabel(c.outcome)} ignored: stale` : 'stale verdict ignored';
		case 'verdict':
			return c.outcome ? `${c.outcome.verifier}: ${verdictLabel(c.outcome)}${c.stale ? ' (stale)' : ''}` : 'verdict';
		default:
			return `${c.action}${by(c)}`;
	}
}

// The line under a history entry: what a set or replace asked for, why it
// ended unverified or what withdrew a candidate, or the verifier's findings.
export function changeDetail(c: VerifyChange): string {
	if (c.action === 'verdict') return oneLine(c.outcome?.findings ?? '', 300);
	const why = c.action === 'exhausted' || c.action === 'withdrawn' || c.action === 'stale';
	return requestText(c) || (why ? (c.reason ?? '') : '');
}

// text on one line, cut to max characters.
function oneLine(text: string, max: number): string {
	const line = text.replace(/\s+/g, ' ').trim();
	return line.length > max ? `${line.slice(0, max - 1)}…` : line;
}

// Whether a verifier's outcome is a stop without a verdict.
export function verdictStopped(o: VerifyOutcome): boolean {
	return o.verdict !== 'PASS' && o.verdict !== 'FAIL';
}

// A verifier's outcome in a word or two: "PASS", "FAIL", "stopped (crashed)".
export function verdictLabel(o: VerifyOutcome): string {
	return verdictStopped(o) ? `stopped (${o.verdict})` : o.verdict;
}

// The round an outcome was told in: "round 1 of 3", or "" when it has none
// (a stop isn't a round, nor a verdict on an earlier conclusion).
export function verdictRound(o: VerifyOutcome): string {
	return o.round && !verdictStopped(o) ? roundOf(o.round, o.max_rounds ?? 0) : '';
}

// Where an outcome stands: "round 1 of 3", "on an earlier conclusion" (told as
// information: it doesn't judge the conclusion pending), "stale: …" (on a
// candidate a newer version or a clear withdrew), or "".
export function verdictContext(o: VerifyOutcome, stale = false): string {
	if (stale) return 'stale: its version was withdrawn';
	return o.earlier ? 'on an earlier conclusion' : verdictRound(o);
}

// A verifier's conclude arguments as a typed verdict, or null when they aren't
// one: no PASS or FAIL, or a result besides (a call the backend refuses).
export function typedVerdict(args: Record<string, unknown> | null): { verdict: string; findings: string } | null {
	if (!args || 'result' in args || typeof args.verdict !== 'string') return null;
	const verdict = args.verdict.trim().toUpperCase();
	if (verdict !== 'PASS' && verdict !== 'FAIL') return null;
	return { verdict, findings: typeof args.findings === 'string' ? args.findings : '' };
}

function upTo(maxRounds: number): string {
	return maxRounds > 0 ? `up to ${maxRounds} ${maxRounds === 1 ? 'round' : 'rounds'}` : 'unbounded rounds';
}

// What a set or replace asked for: "criteria · model · rounds"; "" otherwise.
export function requestText(c: VerifyChange): string {
	if (c.action !== 'set' && c.action !== 'replace') return '';
	return `${c.criteria || "against the agent's current request"} · ${c.model || "the agent's own model"} · ${upTo(c.max_rounds)}`;
}

// A loop's verification of each tick, for its row: "verifies each tick · up
// to 3 rounds · against the tick's prompt", with the verifier's model when it
// isn't the agent's own.
export function eachTickText(v: LoopVerify): string {
	const parts = ['verifies each tick', upTo(v.max_rounds), v.criteria ? `against: ${v.criteria}` : "against the tick's prompt"];
	if (v.model) parts.push(`verifier ${v.model}`);
	return parts.join(' · ');
}
