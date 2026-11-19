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

import { test, mock } from 'node:test';
import assert from 'node:assert/strict';
import { coalescer, Latest } from './refetch.ts';

test('coalescer: debounce waits for quiet, throttle caps the rate', () => {
	mock.timers.enable({ apis: ['setTimeout'] });
	try {
		const runs: string[] = [];
		const d = coalescer((k) => runs.push('d:' + k), 300, 'debounce');
		const t = coalescer((k) => runs.push('t:' + k), 5000, 'throttle');
		for (let i = 0; i < 10; i++) {
			d.schedule('a');
			t.schedule('a');
			mock.timers.tick(200);
		}
		// 2 s of events every 200 ms: debounce hasn't fired, throttle hasn't either
		assert.equal(runs.length, 0);
		mock.timers.tick(300);
		assert.deepEqual(runs, ['d:a']);
		for (let i = 0; i < 20; i++) {
			t.schedule('a');
			t.schedule('b');
			mock.timers.tick(500);
		}
		// 12 s in all: throttle ran each key about every 5 s, not on every event
		assert.deepEqual(runs.filter((r) => r === 't:a').length, 2);
		assert.deepEqual(runs.filter((r) => r === 't:b').length, 2);
		t.schedule('c');
		t.cancel();
		mock.timers.tick(6000);
		assert.equal(runs.includes('t:c'), false);
	} finally {
		mock.timers.reset();
	}
});

test('Latest: only the newest request per key, and none from before a reset', () => {
	const l = new Latest();
	const a1 = l.start('main-agent');
	const a2 = l.start('main-agent');
	const b1 = l.start('worker');
	assert.equal(a1(), false);
	assert.equal(a2(), true);
	assert.equal(b1(), true);
	l.reset(); // another run
	assert.equal(a2(), false);
	assert.equal(b1(), false);
	assert.equal(l.start('main-agent')(), true);
	// the same session id in the next run, its first request each time
	const before = l.start('chatty-bot');
	l.reset();
	const after = l.start('chatty-bot');
	assert.equal(before(), false);
	assert.equal(after(), true);
});
