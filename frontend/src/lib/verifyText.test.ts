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
import {
	changeDetail,
	changeText,
	eachTickText,
	labelOf,
	requestText,
	roundOf,
	typedVerdict,
	verdictContext,
	verdictLabel,
	verdictRound,
	verdictStopped,
	verifyActive,
	verifyState
} from './verifyText.ts';
import type { VerifyChange, VerifyReport } from './types.ts';

const report = (r: Partial<VerifyReport>): VerifyReport => ({
	state: 'off',
	criteria: '',
	model: '',
	max_rounds: 3,
	round: 0,
	verifier: '',
	history: [],
	...r
});
const change = (c: Partial<VerifyChange>): VerifyChange => ({
	action: 'set',
	max_rounds: 0,
	step: 1,
	created_at: '2026-11-10T19:00:00-08:00',
	...c
});

test('verifyState: off, on, verifying (by whom, round n of N), passed, not verified', () => {
	assert.deepEqual(verifyState(report({})), { label: 'off', detail: 'Its conclusions are delivered without a check.' });
	assert.equal(verifyState(report({ state: 'on', round: 1 })).detail, 'Its next conclusion goes to a verifier first.');
	assert.equal(
		verifyState(report({ state: 'on', round: 2 })).detail,
		'Waiting for the next conclusion (round 2 of 3, after 1 rejection).'
	);
	assert.deepEqual(verifyState(report({ state: 'verifying', round: 2, verifier: 'quick-rabbit' })), {
		label: 'verifying',
		detail: 'quick-rabbit is checking its conclusion (round 2 of 3).'
	});
	assert.equal(
		verifyState(report({ state: 'verifying', round: 4, max_rounds: 0, verifier: 'quick-rabbit' })).detail,
		'quick-rabbit is checking its conclusion (round 4).'
	);
	assert.deepEqual(verifyState(report({ state: 'passed', round: 2, verifier: 'quick-rabbit' })), {
		label: 'passed',
		detail: 'Verified by quick-rabbit (round 2).'
	});
	assert.deepEqual(verifyState(report({ state: 'not_verified', round: 3 })), {
		label: 'not verified',
		detail: 'The rounds ran out: 3 rejections.'
	});
	// an end that wasn't the rounds: the record's reason, or a neutral line
	const why = 'the verifier could not be started: no model';
	assert.equal(
		verifyState(report({ state: 'not_verified', round: 0, history: [change({ action: 'exhausted', reason: why })] })).detail,
		`Not verified: ${why}`
	);
	assert.equal(verifyState(report({ state: 'not_verified', round: 0 })).detail, 'It ended without a verdict; see the history.');
});

test('roundOf and verifyActive', () => {
	assert.equal(roundOf(1, 3), 'round 1 of 3');
	assert.equal(roundOf(5, 0), 'round 5');
	assert.equal(verifyActive(report({ state: 'on' })), true);
	assert.equal(verifyActive(report({ state: 'verifying' })), true);
	for (const state of ['off', 'passed', 'not_verified'] as const) assert.equal(verifyActive(report({ state })), false);
	assert.equal(verifyActive(null), false);
});

test('changeText: who did what', () => {
	assert.equal(changeText(change({ action: 'set', by: 'operator' })), 'turned on by the operator');
	assert.equal(changeText(change({ action: 'replace', by: 'main-agent' })), 'replaced by main-agent');
	assert.equal(changeText(change({ action: 'clear', by: 'operator' })), 'turned off by the operator');
	assert.equal(changeText(change({ action: 'verifying', by: 'harness', verifier: 'quick-rabbit', round: 1 })), 'sent to quick-rabbit (round 1)');
	assert.equal(changeText(change({ action: 'rejected', by: 'harness', verifier: 'quick-rabbit', round: 1 })), 'rejected by quick-rabbit (round 1)');
	assert.equal(changeText(change({ action: 'passed', by: 'harness', verifier: 'quick-rabbit', round: 2 })), 'passed: verified by quick-rabbit (round 2)');
	assert.equal(changeText(change({ action: 'exhausted', by: 'harness', round: 3 })), 'not verified (round 3)');
	assert.equal(changeText(change({ action: 'replace', by: 'loop 2' })), 'replaced by loop 2'); // a loop's tick started it
});

test("eachTickText: a loop's verification of each tick", () => {
	assert.equal(eachTickText({ max_rounds: 3 }), "verifies each tick · up to 3 rounds · against the tick's prompt");
	assert.equal(
		eachTickText({ criteria: 'the tests pass', model: 'vertex:judge', max_rounds: 1 }),
		'verifies each tick · up to 1 round · against: the tests pass · verifier vertex:judge'
	);
	assert.equal(eachTickText({ max_rounds: 0 }), "verifies each tick · unbounded rounds · against the tick's prompt");
});

test('versions: the state and each change say which version and candidate', () => {
	assert.equal(labelOf(2, 1), 'v2, candidate 1');
	assert.equal(labelOf(2), 'v2');
	assert.equal(labelOf(0, 3), '');
	assert.equal(verifyState(report({ state: 'on', round: 1, version: 2 })).detail, 'Its next conclusion goes to a verifier first (v2).');
	assert.equal(
		verifyState(report({ state: 'verifying', round: 1, verifier: 'quick-rabbit', version: 2, candidate: 1 })).detail,
		'quick-rabbit is checking its conclusion (v2, candidate 1; round 1 of 3).'
	);
	assert.equal(
		verifyState(report({ state: 'on', round: 2, version: 3 })).detail,
		'Waiting for the next conclusion (v3, round 2 of 3, after 1 rejection).'
	);
	assert.equal(changeText(change({ action: 'replace', by: 'operator', version: 2 })), 'v2 · replaced by the operator');
	assert.equal(
		changeText(change({ action: 'verifying', verifier: 'quick-rabbit', round: 1, version: 2, candidate: 1 })),
		'v2, candidate 1 · sent to quick-rabbit (round 1)'
	);
	const withdrawn = change({ action: 'withdrawn', version: 1, candidate: 1, reason: 'v1, candidate 1 was superseded by v2' });
	assert.equal(changeText(withdrawn), 'v1, candidate 1 · withdrawn: never delivered');
	assert.equal(changeDetail(withdrawn), 'v1, candidate 1 was superseded by v2');
	const pass = { verifier: 'quick-rabbit', verdict: 'PASS' };
	assert.equal(changeText(change({ action: 'verdict', outcome: pass, version: 1, candidate: 1, stale: true })), 'v1, candidate 1 · quick-rabbit: PASS (stale)');
	const stale = change({ action: 'stale', outcome: pass, version: 1, candidate: 1, reason: 'for v1, candidate 1, which v2 superseded' });
	assert.equal(changeText(stale), "v1, candidate 1 · quick-rabbit's PASS ignored: stale");
	assert.equal(changeDetail(stale), 'for v1, candidate 1, which v2 superseded');
});

test('requestText: what a set or replace asked for', () => {
	assert.equal(requestText(change({ action: 'set', criteria: 'tests pass', max_rounds: 3 })), "tests pass · the agent's own model · up to 3 rounds");
	assert.equal(
		requestText(change({ action: 'replace', model: 'vertex:x', max_rounds: 1 })),
		"against the agent's current request · vertex:x · up to 1 round"
	);
	assert.equal(requestText(change({ action: 'set' })), "against the agent's current request · the agent's own model · unbounded rounds");
	assert.equal(requestText(change({ action: 'verifying' })), '');
	assert.equal(changeDetail(change({ action: 'exhausted', reason: 'the verifier stopped 3 times' })), 'the verifier stopped 3 times');
	assert.equal(changeDetail(change({ action: 'set', criteria: 'x', max_rounds: 1 })), "x · the agent's own model · up to 1 round");
	assert.equal(changeDetail(change({ action: 'rejected', reason: 'n/a' })), '');
});

test('a verifier outcome: its own record in the history, and as told', () => {
	const fail = { verifier: 'quick-rabbit', verdict: 'FAIL', findings: 'no unit', round: 1, max_rounds: 3 };
	const stop = { verifier: 'quick-rabbit', verdict: 'crashed', findings: 'llm call: 404' };
	assert.equal(changeText(change({ action: 'verdict', outcome: fail })), 'quick-rabbit: FAIL');
	assert.equal(changeDetail(change({ action: 'verdict', outcome: fail })), 'no unit');
	assert.equal(changeText(change({ action: 'verdict', outcome: stop })), 'quick-rabbit: stopped (crashed)');
	assert.equal(changeDetail(change({ action: 'verdict', outcome: stop })), 'llm call: 404');
	const long = changeDetail(change({ action: 'verdict', outcome: { ...fail, findings: 'a\n\n' + 'b'.repeat(400) } }));
	assert.equal(long.length, 300);
	assert.ok(long.startsWith('a b') && long.endsWith('…'));
	assert.equal(verdictLabel({ verifier: 'v', verdict: 'PASS' }), 'PASS');
	assert.equal(verdictStopped(stop), true);
	assert.equal(verdictStopped(fail), false);
	assert.equal(verdictRound(fail), 'round 1 of 3');
	assert.equal(verdictRound({ ...fail, max_rounds: 0, round: 4 }), 'round 4');
	assert.equal(verdictRound(stop), '');
	assert.equal(verdictRound({ ...stop, round: 1, max_rounds: 3 }), '', 'a stop shows no round');
	assert.equal(verdictContext(fail), 'round 1 of 3');
	assert.equal(verdictContext({ verifier: 'v', verdict: 'FAIL', findings: 'x', earlier: true }), 'on an earlier conclusion');
	assert.equal(verdictContext({ verifier: 'v', verdict: 'PASS', earlier: true }, true), 'stale: its version was withdrawn');
});

test('typedVerdict: PASS or FAIL only, and not beside a result', () => {
	assert.deepEqual(typedVerdict({ verdict: 'fail', findings: 'no unit' }), { verdict: 'FAIL', findings: 'no unit' });
	assert.deepEqual(typedVerdict({ verdict: 'PASS' }), { verdict: 'PASS', findings: '' });
	assert.equal(typedVerdict({ verdict: 'MAYBE', findings: 'x' }), null);
	assert.equal(typedVerdict({ verdict: 'PASS', findings: 'x', result: 'PASS' }), null);
	assert.equal(typedVerdict({ result: 'It is 42.' }), null);
	assert.equal(typedVerdict(null), null);
});
