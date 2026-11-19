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

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { formatAhead } from './loopSchedule.ts';
import { activeLoops, activeVerify, loopsLabel, shownVerify, stateMarks, verifyBadge, verifyLabel, verifyStatusText } from './sessionState.ts';
import type { SessionVerify } from './types.ts';

const LA = 'America/Los_Angeles';
const NOW = Date.UTC(2026, 10, 10, 23, 30, 15); // Tue Nov 10 15:30:15 PST
const v = (status: SessionVerify['status'], x: Partial<SessionVerify> = {}): SessionVerify => ({
	status,
	version: 2,
	round: 1,
	max_rounds: 3,
	verifier: 'quick-rabbit',
	...x
});

test('verifyLabel: the version, the state, the round and who is checking', () => {
	assert.equal(verifyLabel(v('verifying')), 'Verification v2: verifying · round 1 of 3 · verifier quick-rabbit');
	assert.equal(verifyLabel(v('on')), 'Verification v2: on · round 1 of 3');
	assert.equal(verifyLabel(v('on', { round: 2, max_rounds: undefined })), 'Verification v2: on · round 2, no limit');
	assert.equal(verifyLabel(v('verifying', { max_rounds: 0 })), 'Verification v2: verifying · round 1, no limit · verifier quick-rabbit');
	assert.equal(verifyLabel(v('passed', { version: 1 })), 'Verification v1: passed · round 1 of 3 · verifier quick-rabbit');
	assert.equal(verifyLabel(v('not_verified', { round: 3 })), 'Verification v2: not verified · round 3 of 3 · verifier quick-rabbit');
	// An ended one reads against its last request's limit, or its lack of one.
	assert.equal(verifyLabel(v('passed', { max_rounds: 0, round: 2 })), 'Verification v2: passed · round 2, no limit · verifier quick-rabbit');
	assert.equal(verifyLabel(v('on', { round: undefined, verifier: undefined })), 'Verification v2: on');
});

test('verifyBadge: short, for a status line', () => {
	assert.equal(verifyBadge(v('verifying')), 'verifying · round 1/3');
	assert.equal(verifyBadge(v('on')), 'verification on · round 1/3');
	assert.equal(verifyBadge(v('verifying', { round: 2, max_rounds: 0 })), 'verifying · round 2');
	assert.equal(verifyBadge(v('passed')), 'passed · round 1/3');
	assert.equal(verifyBadge(v('passed', { max_rounds: 0, round: 2 })), 'passed · round 2');
	assert.equal(verifyBadge(v('not_verified', { round: 3 })), 'not verified · round 3/3');
});

test('loopsLabel: the count and the soonest next tick, short', () => {
	const when = (ms: number) => formatAhead(ms, LA, NOW);
	assert.equal(loopsLabel({ count: 1, next_tick: '2026-11-11T01:00:00Z' }, when), '1 loop · next 17:00');
	assert.equal(loopsLabel({ count: 2, next_tick: '2026-11-12T17:00:00Z' }, when), '2 loops · next Thu 09:00');
	assert.equal(loopsLabel({ count: 2, next_tick: '2026-11-30T17:00:00Z' }, when), '2 loops · next Nov 30');
	assert.equal(loopsLabel({ count: 3, next_tick: '0001-01-01T00:00:00Z' }, when), '3 loops'); // none has a next tick
});

test('what shows: icons only while active, a status line also an outcome, never verification on an interactive agent', () => {
	assert.equal(activeVerify({ verify: v('verifying') })?.status, 'verifying');
	assert.equal(activeVerify({ verify: v('on') })?.status, 'on');
	for (const status of ['passed', 'not_verified', 'off'] as const) assert.equal(activeVerify({ verify: v(status) }), null);
	assert.equal(activeVerify({}), null);
	assert.equal(activeVerify({ interactive: true, verify: v('verifying') }), null);
	assert.equal(shownVerify({ verify: v('passed') })?.status, 'passed');
	assert.equal(shownVerify({ verify: v('not_verified') })?.status, 'not_verified');
	assert.equal(shownVerify({ verify: v('off') }), null);
	assert.equal(shownVerify({ interactive: true, verify: v('on') }), null);
	assert.equal(activeLoops({ loops: { count: 1, next_tick: '' } })?.count, 1);
	assert.equal(activeLoops({ loops: { count: 0, next_tick: '' } }), null);
	assert.equal(activeLoops({}), null);
});

test('stateMarks: plain marks for a native select', () => {
	const loops = { count: 1, next_tick: '2026-11-11T01:00:00Z' };
	assert.equal(stateMarks({}), '');
	assert.equal(stateMarks({ verify: v('verifying') }), '◆ ');
	assert.equal(stateMarks({ loops }), '↻ ');
	assert.equal(stateMarks({ verify: v('on'), loops }), '◆ ↻ ');
	assert.equal(stateMarks({ verify: v('passed'), loops }), '↻ ');
	assert.equal(stateMarks({ interactive: true, verify: v('on'), loops }), '↻ ');
});

test('verifyStatusText: the status in words, as the Inspector lists it', () => {
	assert.equal(verifyStatusText('on'), 'on');
	assert.equal(verifyStatusText('verifying'), 'verifying');
	assert.equal(verifyStatusText('passed'), 'passed');
	assert.equal(verifyStatusText('not_verified'), 'not verified');
});
