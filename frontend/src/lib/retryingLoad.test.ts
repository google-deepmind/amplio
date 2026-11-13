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
import { RetryingLoad } from './retryingLoad.ts';

// attempts answers with outcomes in turn (true: succeed), counting calls.
function attempts(...outcomes: boolean[]): { calls: number; fn: () => Promise<void> } {
	const counter = {
		calls: 0,
		fn: (): Promise<void> => {
			const ok = outcomes[counter.calls++] ?? true;
			return ok ? Promise.resolve() : Promise.reject(new Error('unreachable'));
		}
	};
	return counter;
}

// settle lets the attempt's promise callbacks run.
const settle = () => new Promise<void>((r) => setImmediate(r));

test('a failed load is tried again after a delay that doubles, up to the cap', async (t) => {
	t.mock.timers.enable({ apis: ['setTimeout'] });
	const a = attempts(false, false, false, false);
	const loader = new RetryingLoad(a.fn, 1_000, 3_000);
	await loader.load();
	assert.equal(a.calls, 1);
	for (const delay of [1_000, 2_000, 3_000, 3_000]) {
		t.mock.timers.tick(delay - 1);
		await settle();
		const before: number = a.calls;
		t.mock.timers.tick(1);
		await settle();
		assert.equal(a.calls, before + 1, `retry after ${delay} ms`);
	}
	assert.ok(loader.done);
	t.mock.timers.tick(60_000);
	await settle();
	assert.equal(a.calls, 5, 'no attempt after one succeeded');
	await loader.load();
	assert.equal(a.calls, 5, 'load() after success is a no-op');
});

test('load() while a retry is scheduled tries at once, in place of that retry', async (t) => {
	t.mock.timers.enable({ apis: ['setTimeout'] });
	const a = attempts(false, false, true);
	const loader = new RetryingLoad(a.fn, 10_000);
	await loader.load(); // fails: a retry in 10 s
	await loader.load(); // say, the server is reachable again: fails, a retry in 20 s
	assert.equal(a.calls, 2);
	t.mock.timers.tick(10_000);
	await settle();
	assert.equal(a.calls, 2, 'the retry it replaced was dropped');
	t.mock.timers.tick(10_000);
	await settle();
	assert.equal(a.calls, 3);
	assert.ok(loader.done);
});

test('concurrent loads share one attempt', async () => {
	const releases: (() => void)[] = [];
	const loader = new RetryingLoad(() => new Promise<void>((r) => releases.push(r)));
	const first = loader.load();
	const second = loader.load();
	releases.forEach((r) => r());
	await Promise.all([first, second]);
	assert.equal(releases.length, 1);
	assert.ok(loader.done);
});
