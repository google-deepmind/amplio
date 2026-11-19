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

// What a session's state icons and status-line badges show of its
// verification (SessionDTO.verify) and loops (SessionDTO.loops), and when.
// Pure, so `npm test` can exercise it in node.

import type { SessionDTO, SessionLoops, SessionVerify } from './types.ts';

type Session = Pick<SessionDTO, 'interactive' | 'verify' | 'loops'>;

// A verification in force (its next conclusion goes to a verifier, or one is
// with it), for the icons. An interactive agent is never verified.
export function activeVerify(s: Session): SessionVerify | null {
	const v = s.verify;
	return !s.interactive && v && inForce(v) ? v : null;
}

// What a status line shows: one in force, or how the latest ended.
export function shownVerify(s: Session): SessionVerify | null {
	const v = s.verify;
	return !s.interactive && v && v.status !== 'off' ? v : null;
}

export function activeLoops(s: Session): SessionLoops | null {
	return s.loops && s.loops.count > 0 ? s.loops : null;
}

const STATUS: Record<SessionVerify['status'], string> = {
	on: 'on',
	verifying: 'verifying',
	passed: 'passed',
	not_verified: 'not verified',
	off: 'off'
};

// A verification status in words: "on", "verifying", "passed", "not verified", "off".
export function verifyStatusText(status: SessionVerify['status']): string {
	return STATUS[status];
}

// "round 1 of 3" ("round 2, no limit"), short "round 1/3" ("round 2"), against
// the request in force or, once ended, the last one; "" before a round.
function roundText(v: SessionVerify, short: boolean): string {
	const round = v.round ?? 0;
	const max = v.max_rounds ?? 0;
	if (!round) return '';
	if (short) return max ? `round ${round}/${max}` : `round ${round}`;
	return max ? `round ${round} of ${max}` : `round ${round}, no limit`;
}

function inForce(v: SessionVerify): boolean {
	return v.status === 'on' || v.status === 'verifying';
}

// The tooltip and accessible name: "Verification v2: verifying · round 1 of
// 3 · verifier quick-rabbit" (one in force has no verifier at work yet).
export function verifyLabel(v: SessionVerify): string {
	const verifier = v.status !== 'on' && v.verifier ? `verifier ${v.verifier}` : '';
	return `Verification v${v.version}: ${[STATUS[v.status], roundText(v, false), verifier].filter(Boolean).join(' · ')}`;
}

// A status line's badge: "verifying · round 1/3", "verification on · round 1/3".
export function verifyBadge(v: SessionVerify): string {
	const what = v.status === 'on' ? 'verification on' : STATUS[v.status];
	return [what, roundText(v, true)].filter(Boolean).join(' · ');
}

// "2 loops · next 15:30", `when` wording the soonest next tick; the count
// alone when none has one (the zero time).
export function loopsLabel(l: SessionLoops, when: (ms: number) => string): string {
	const head = l.count === 1 ? '1 loop' : `${l.count} loops`;
	const next = Date.parse(l.next_tick);
	return next > 0 ? `${head} · next ${when(next)}` : head;
}

// Plain-text marks for where icons can't go (a native <select>'s options).
export function stateMarks(s: Session): string {
	return (activeVerify(s) ? '◆ ' : '') + (activeLoops(s) ? '↻ ' : '');
}
