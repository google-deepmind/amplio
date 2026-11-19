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
import { addRootLoop, deliver, type SendApi } from './composerSend.ts';
import type { LoopFields } from './loopSchedule.ts';
import type { LoopDTO } from './types.ts';

const NOW = Date.UTC(2026, 10, 10, 23, 30, 15, 500);

// A recording stand-in for lib/api; roots lists what successive getRun calls name.
function fakeApi(roots: string[] = ['main-agent']) {
	const calls: unknown[][] = [];
	const api: SendApi = {
		sendMessage: async (...a) => void calls.push(['sendMessage', ...a]),
		addLoop: async (...a) => {
			calls.push(['addLoop', ...a]);
			return { id: '1' } as LoopDTO;
		},
		getRun: async (...a) => {
			calls.push(['getRun', ...a]);
			return { root_session_id: roots.shift() ?? '' };
		}
	};
	return { api, calls };
}
const loop = (f: Partial<LoopFields> = {}): LoopFields => ({ every: 30, unit: 'minutes', sendNow: true, ends: { kind: 'times', n: 3 }, ...f });

test('deliver: a message carries its verification', async () => {
	const { api, calls } = fakeApi();
	const sent = await deliver(api, 'r1', 'main-agent', 'fix it\n', { verify: { criteria: ' tests pass ', model: '', max_rounds: 2 }, loop: null }, NOW);
	assert.deepEqual(sent, { kind: 'message' });
	assert.deepEqual(calls, [['sendMessage', 'r1', 'main-agent', 'fix it\n', { criteria: 'tests pass', max_rounds: 2 }]]);
});

test('deliver: a plain message has no verify', async () => {
	const { api, calls } = fakeApi();
	await deliver(api, 'r1', 'chatty-bot', 'hi', { verify: null, loop: null }, NOW);
	assert.deepEqual(calls, [['sendMessage', 'r1', 'chatty-bot', 'hi', undefined]]);
});

test('deliver: a repeat creates the loop instead of posting the message', async () => {
	const { api, calls } = fakeApi();
	const sent = await deliver(api, 'r1', 'chatty-bot', '  check the eval\n\n', { verify: null, loop: loop() }, NOW);
	assert.equal(sent.kind, 'loop');
	assert.deepEqual(calls, [
		['addLoop', 'r1', 'chatty-bot', 'check the eval', { kind: 'rrule', spec: 'DTSTART:20261110T233015Z\nRRULE:FREQ=MINUTELY;INTERVAL=30;COUNT=3' }, undefined]
	]);
});

test('deliver: verify with a repeat is a loop that verifies each tick, and no message', async () => {
	const { api, calls } = fakeApi();
	const sent = await deliver(api, 'r1', 'main-agent', 'run the tests', { verify: { criteria: '', model: '', max_rounds: 2 }, loop: loop() }, NOW);
	assert.equal(sent.kind, 'loop');
	assert.deepEqual(calls, [
		['addLoop', 'r1', 'main-agent', 'run the tests', { kind: 'rrule', spec: 'DTSTART:20261110T233015Z\nRRULE:FREQ=MINUTELY;INTERVAL=30;COUNT=3' }, { max_rounds: 2 }]
	]);
});

test('addRootLoop waits for the run to name its root, then adds the loop one interval out', async () => {
	const { api, calls } = fakeApi(['', '', 'main-agent']);
	await addRootLoop(api, 'r1', ' the task ', loop({ sendNow: false, every: 1, unit: 'hours' }), null, () => NOW, { tries: 5, every: 1 });
	assert.deepEqual(calls, [
		['getRun', 'r1'],
		['getRun', 'r1'],
		['getRun', 'r1'],
		['addLoop', 'r1', 'main-agent', 'the task', { kind: 'rrule', spec: 'DTSTART:20261111T003015Z\nRRULE:FREQ=HOURLY;INTERVAL=1;COUNT=3' }, undefined]
	]);
});

test("addRootLoop sends the loop's own verification of each tick", async () => {
	const { api, calls } = fakeApi();
	await addRootLoop(api, 'r1', 'the task', loop({ sendNow: false }), { criteria: ' it builds ', model: '', max_rounds: 1 }, () => NOW);
	assert.deepEqual(calls.at(-1)?.at(-1), { criteria: 'it builds', max_rounds: 1 });
});

test('addRootLoop gives up when the root never appears', async () => {
	const { api, calls } = fakeApi([]);
	await assert.rejects(addRootLoop(api, 'r1', 'p', loop({ sendNow: false }), null, () => NOW, { tries: 3, every: 1 }), /didn't start/);
	assert.deepEqual(calls, [['getRun', 'r1'], ['getRun', 'r1'], ['getRun', 'r1']]);
});
