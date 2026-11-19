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
	blockedOption,
	loopChip,
	loopVerifyFields,
	newVerifyFields,
	optionsNote,
	slashShortcut,
	TASK_SUPERSEDED,
	tickCostNote,
	validateVerify,
	verifyChip,
	verifyPatch,
	verifyRequest
} from './composerOptions.ts';
import type { LoopFields } from './loopSchedule.ts';

const LA = 'America/Los_Angeles';
const NOW = Date.UTC(2026, 10, 10, 23, 30, 15); // Tue Nov 10 15:30:15 PST

test('a new verification leaves every default to the server, and its chip shows the server\'s rounds', () => {
	assert.deepEqual(verifyRequest(newVerifyFields()), {});
	assert.equal(verifyChip(newVerifyFields(), 5), 'Verify · 5 rounds');
});

test('verifyRequest sends what the form shows, and leaves blanks to the server', () => {
	assert.deepEqual(verifyRequest({ criteria: '  tests pass ', model: 'vertex:x', max_rounds: 3 }), {
		criteria: 'tests pass',
		model: 'vertex:x',
		max_rounds: 3
	});
	assert.deepEqual(verifyRequest({ criteria: ' ', model: '', max_rounds: null }), {});
	assert.deepEqual(verifyRequest({ criteria: '', model: '', max_rounds: 0 }), { max_rounds: 0 });
});

test('validateVerify', () => {
	assert.equal(validateVerify({ criteria: '', model: '', max_rounds: 0 }), null);
	assert.equal(validateVerify({ criteria: '', model: '', max_rounds: null }), null);
	assert.match(validateVerify({ criteria: '', model: '', max_rounds: -1 }) ?? '', /whole number/);
	assert.match(validateVerify({ criteria: '', model: '', max_rounds: 1.5 }) ?? '', /whole number/);
});

test('verifyChip', () => {
	assert.equal(verifyChip({ criteria: '', model: '', max_rounds: 3 }, 3), 'Verify · 3 rounds');
	assert.equal(verifyChip({ criteria: '', model: '', max_rounds: 1 }, 3), 'Verify · 1 round');
	assert.equal(verifyChip({ criteria: '', model: '', max_rounds: 0 }, 3), 'Verify · unbounded');
	assert.equal(verifyChip({ criteria: '', model: '', max_rounds: null }, 5), 'Verify · 5 rounds');
	assert.equal(verifyChip({ criteria: '', model: '', max_rounds: 2 }, 3, true), 'Verify each tick · 2 rounds');
});

test("tickCostNote: each tick replaces the session's verification, up to max rounds verifier runs", () => {
	assert.equal(tickCostNote(3), "Each tick starts a new verification, replacing the session's current one: up to 3 verifier runs per tick.");
	assert.equal(tickCostNote(1), "Each tick starts a new verification, replacing the session's current one: up to 1 verifier run per tick.");
	assert.match(tickCostNote(0), /replacing the session's current one, with no limit on verifier runs per tick/);
});

test('optionsNote: the cost of verifying each tick, unless its form says so; on the new-run form, the task superseded', () => {
	const v = { criteria: '', model: '', max_rounds: null };
	const l: LoopFields = { every: 1, unit: 'hours', sendNow: true, ends: { kind: 'never' } };
	const s = (x: Partial<Parameters<typeof optionsNote>[0]>) => ({
		verify: v,
		loop: l,
		loopVerify: null,
		open: null,
		ownLoopVerify: false,
		...x
	});
	assert.equal(optionsNote(s({}), 3), tickCostNote(3));
	assert.equal(optionsNote(s({ verify: { ...v, max_rounds: 1 } }), 3), tickCostNote(1));
	assert.equal(optionsNote(s({ open: 'loop' }), 3), tickCostNote(3));
	assert.equal(optionsNote(s({ open: 'verify' }), 3), '');
	assert.equal(optionsNote(s({ loop: null }), 3), '');
	assert.equal(optionsNote(s({ verify: null }), 3), '');
	assert.equal(optionsNote(s({ ownLoopVerify: true }), 3), ''); // the task's Verify and a loop that verifies nothing
	assert.equal(optionsNote(s({ ownLoopVerify: true, loopVerify: v }), 3), TASK_SUPERSEDED);
	assert.equal(optionsNote(s({ ownLoopVerify: true, loopVerify: v, open: 'verify' }), 3), '');
});

test("loopVerifyFields and verifyPatch: an edit sends the loop's verification only when it changed", () => {
	const fields = loopVerifyFields({ max_rounds: 3, model: 'vertex:judge' });
	assert.deepEqual(fields, { criteria: '', model: 'vertex:judge', max_rounds: 3 });
	assert.equal(loopVerifyFields(undefined), null);
	assert.equal(verifyPatch(fields, { criteria: '', model: 'vertex:judge', max_rounds: 3 }), undefined);
	assert.equal(verifyPatch(null, null), undefined);
	assert.deepEqual(verifyPatch(fields, { criteria: ' they pass ', model: 'vertex:judge', max_rounds: 3 }), {
		criteria: 'they pass',
		model: 'vertex:judge',
		max_rounds: 3
	});
	assert.deepEqual(verifyPatch(null, { criteria: '', model: '', max_rounds: 0 }), { max_rounds: 0 });
	assert.equal(verifyPatch(fields, null), null); // stop verifying
});

test('loopChip', () => {
	const f = (x: Partial<LoopFields>): LoopFields => ({
		every: 30,
		unit: 'minutes',
		sendNow: true,
		ends: { kind: 'after', n: 7, unit: 'days' },
		...x
	});
	assert.equal(loopChip(f({}), LA, NOW), 'Repeat every 30m · until Nov 17');
	assert.equal(loopChip(f({ ends: { kind: 'after', n: 1, unit: 'hours' } }), LA, NOW), 'Repeat every 30m · until 16:30');
	assert.equal(loopChip(f({ every: 1, unit: 'hours', ends: { kind: 'times', n: 5 } }), LA, NOW), 'Repeat every 1h · 5 times');
	assert.equal(loopChip(f({ ends: { kind: 'times', n: 1 } }), LA, NOW), 'Repeat every 30m · once');
	assert.equal(loopChip(f({ every: 2, unit: 'days', ends: { kind: 'never' } }), LA, NOW), 'Repeat every 2d · no end');
	assert.equal(loopChip(f({ every: 90 }), LA, NOW), 'Repeat every 90m · until Nov 17');
	assert.equal(loopChip(f({ sendNow: false, ends: { kind: 'times', n: 5 } }), LA, NOW), 'Repeat every 30m · 5 times · first in 30m');
	assert.equal(loopChip(f({ sendNow: false }), LA, NOW), 'Repeat every 30m · until Nov 17 · first in 30m');
	assert.equal(loopChip(f({ ends: { kind: 'times', n: 2 } }), LA, NOW, true), 'Repeat every 30m · 2 times · verify each tick');
});

test('slashShortcut: the first word opens a form and is removed; nothing else is parsed', () => {
	assert.deepEqual(slashShortcut('/loop '), { cmd: 'loop', rest: '' });
	assert.deepEqual(slashShortcut('/loop check the eval every 30m'), { cmd: 'loop', rest: 'check the eval every 30m' });
	assert.deepEqual(slashShortcut('/verify\ntests pass'), { cmd: 'verify', rest: 'tests pass' });
	assert.equal(slashShortcut('/loop'), null); // still typing
	assert.deepEqual(slashShortcut('/loop', true), { cmd: 'loop', rest: '' });
	assert.equal(slashShortcut('/loops are fun'), null);
	assert.equal(slashShortcut(' /loop x'), null);
	assert.equal(slashShortcut('please /loop x'), null);
	assert.equal(slashShortcut('/LOOP x'), null);
	assert.equal(slashShortcut('/remind x'), null);
});

test('blockedOption: verify only where the target concludes; with a repeat it verifies each tick', () => {
	// A composer's whole state, as ComposerState passes itself.
	const state = (offersVerify: boolean, verify: unknown, loop: unknown) => ({ offersVerify, verify, loop });
	const v = { criteria: '', model: '', max_rounds: 3 };
	const l = { every: 1, unit: 'hours', sendNow: true, ends: { kind: 'never' } };
	assert.equal(blockedOption('verify', state(true, null, null)), '');
	assert.equal(blockedOption('verify', state(true, null, l)), '');
	assert.equal(blockedOption('loop', state(true, v, null)), '');
	assert.equal(blockedOption('loop', state(false, null, null)), '');
	assert.match(blockedOption('verify', state(false, null, l)), /^Interactive sessions cannot be verified.*use the Inspector tab\.$/);
});
