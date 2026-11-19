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
import { importRunes } from './testing/runes.ts';
import type { VerifyFields } from './composerOptions.ts';
import { deliver, type SendApi } from './composerSend.ts';
import type { LoopFields } from './loopSchedule.ts';
import type { LoopDTO } from './types.ts';
import type * as Composer from './composer.svelte.ts';

const { ComposerState } = await importRunes<typeof Composer>(new URL('./composer.svelte.ts', import.meta.url));

const VERIFY: VerifyFields = { criteria: 'tests pass', model: '', max_rounds: 2 };
const LOOP: LoopFields = { every: 30, unit: 'minutes', sendNow: true, ends: { kind: 'times', n: 5 } };
const plain = <T>(v: T): T => JSON.parse(JSON.stringify(v));

// The discard confirm, answered `answer`; returns how often it was asked.
function confirms(answer: boolean): { asked: number } {
	const n = { asked: 0 };
	globalThis.confirm = () => (n.asked++, answer);
	return n;
}

test('a failed send puts its options back', () => {
	const c = new ComposerState(() => true);
	c.verify = { ...VERIFY };
	const taken = c.take();
	assert.equal(c.verify, null);
	c.restore(taken);
	assert.deepEqual(plain(c.verify), VERIFY);
});

test("a failed send's options don't come back after the composer was reset for another run or session", () => {
	const c = new ComposerState(() => true);
	c.loop = { ...LOOP };
	const taken = c.take();
	c.reset(); // the chat switched sessions, the Overview runs
	c.restore(taken);
	assert.equal(c.loop, null);
	assert.equal(c.verify, null);
});

test('an option chosen anew while the send was in flight wins over the one it took', () => {
	const c = new ComposerState(() => true);
	c.loop = { ...LOOP };
	const taken = c.take();
	const meanwhile: LoopFields = { ...LOOP, every: 2, unit: 'hours' };
	c.loop = meanwhile;
	c.restore(taken);
	assert.deepEqual(plain(c.loop), meanwhile);
});

test('restore is per option: a Repeat added during a failing send keeps its Verify, so the retry verifies each tick', async () => {
	const c = new ComposerState(() => true);
	c.verify = { ...VERIFY };
	const taken = c.take();
	c.loop = { ...LOOP }; // added while the send is in flight
	c.restore(taken);
	assert.deepEqual(plain(c.verify), VERIFY);
	assert.deepEqual(plain(c.loop), LOOP);
	const calls: unknown[][] = [];
	const api: SendApi = {
		sendMessage: async (...a) => void calls.push(['sendMessage', ...a]),
		addLoop: async (...a) => (calls.push(['addLoop', ...a]), { id: '1' } as LoopDTO),
		getRun: async () => ({ root_session_id: '' })
	};
	await deliver(api, 'r', 's', 'run the tests', c.take(), Date.UTC(2026, 10, 12));
	assert.deepEqual(calls.map((x) => [x[0], plain(x[5])]), [['addLoop', { criteria: 'tests pass', max_rounds: 2 }]]);
});

test('opening the form that is already open keeps its unapplied edits: the next switch still asks', () => {
	const n = confirms(false);
	const c = new ComposerState(() => true);
	assert.equal(c.show('loop'), true);
	c.dirty = true; // LoopForm's ondirty
	assert.equal(c.slash('/loop '), ''); // the shortcut typed again over it
	assert.equal(c.show('loop'), true); // or its chip clicked
	assert.equal(c.dirty, true);
	assert.equal(c.show('verify'), false);
	assert.equal(n.asked, 1);
	assert.equal(c.open, 'loop');
});

test("while typing, a shortcut doesn't open over another form's unapplied edits; sending asks once", () => {
	const n = confirms(false);
	const c = new ComposerState(() => true);
	c.show('verify');
	c.dirty = true;
	for (const text of ['/loop ', '/loop c', '/loop ch', '/loop che', '/loop chec', '/loop check']) {
		assert.equal(c.slash(text), false); // the box keeps the text as typed
	}
	assert.equal(n.asked, 0);
	assert.equal(c.hint, 'Add or cancel the open form first.');
	assert.equal(c.open, 'verify');
	assert.equal(c.slash('/loop check', true), false); // Send: asked, and declined
	assert.equal(n.asked, 1);
	assert.equal(c.open, 'verify');
	confirms(true);
	assert.equal(c.slash('/loop check', true), 'check'); // Send again, discarding the edits
	assert.equal(c.open, 'loop');
});

test('without unapplied edits, a shortcut switches forms as it is typed', () => {
	const n = confirms(false);
	const c = new ComposerState(() => true);
	c.show('verify');
	assert.equal(c.slash('/loop check'), 'check');
	assert.equal(c.open, 'loop');
	assert.equal(n.asked, 0);
});

test("a click outside the open form closes it, but doesn't drop its unapplied edits unasked", () => {
	const n = confirms(false);
	const c = new ComposerState(() => true);
	assert.equal(c.dismiss(), true); // nothing open
	c.show('verify');
	assert.equal(c.dismiss(), true);
	assert.equal(c.open, null);
	c.show('loop');
	c.dirty = true;
	assert.equal(c.dismiss(), false); // a popover: it stays, and says why
	assert.equal(c.open, 'loop');
	assert.equal(c.hint, 'Add or cancel the open form first.');
	assert.equal(n.asked, 0);
	assert.equal(c.dismiss(true), false); // a sheet's scrim: asked, and declined
	assert.equal(n.asked, 1);
	assert.equal(c.open, 'loop');
	confirms(true);
	assert.equal(c.dismiss(true), true);
	assert.equal(c.open, null);
	assert.equal(c.dirty, false);
	assert.equal(c.hint, '');
});
