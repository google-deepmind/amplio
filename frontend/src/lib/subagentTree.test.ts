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
import { subagentTree } from './subagentTree.ts';
import type { SessionDTO } from './types.ts';

type S = Pick<SessionDTO, 'session_id' | 'parent_id' | 'status' | 'interactive' | 'verify' | 'loops'>;
const s = (session_id: string, parent_id: string, status: string, x: Partial<S> = {}): S => ({
	session_id,
	parent_id,
	status,
	interactive: false,
	...x
});
const ids = (t: ReturnType<typeof subagentTree<S>>, parent: string) => (t.kept.get(parent) ?? []).map((x) => x.session_id);
const loops = { count: 1, next_tick: '2026-11-17T23:00:00Z' };

test('subagentTree: running sub-agents, and the parents that lead to them', () => {
	const t = subagentTree(
		[
			s('bot', '', 'awaiting'),
			s('done', 'bot', 'concluded'),
			s('crashed', 'bot', 'crashed'),
			s('orphan', 'crashed', 'ongoing'),
			s('worker', 'bot', 'ongoing')
		],
		'bot'
	);
	assert.deepEqual(ids(t, 'bot'), ['crashed', 'worker']);
	assert.deepEqual(ids(t, 'crashed'), ['orphan']);
	assert.equal(t.live, 2);
	assert.equal(t.looping, 0);
	assert.equal(t.depth, 2);
	assert.equal(t.rows, true);
});

test('subagentTree: a sub-agent idle between loop ticks is listed, as looping', () => {
	const t = subagentTree(
		[s('bot', '', 'awaiting'), s('ticker', 'bot', 'concluded', { loops }), s('idle', 'bot', 'concluded', { loops: { count: 0, next_tick: '' } })],
		'bot'
	);
	assert.deepEqual(ids(t, 'bot'), ['ticker']);
	assert.equal(t.live, 0);
	assert.equal(t.looping, 1);
	assert.equal(t.rows, true);

	// Running with loops counts as running, once.
	const busy = subagentTree([s('bot', '', 'awaiting'), s('ticker', 'bot', 'ongoing', { loops })], 'bot');
	assert.equal(busy.live, 1);
	assert.equal(busy.looping, 0);
});

test('subagentTree: nothing to show, and a parent cycle does not hang', () => {
	const none = subagentTree([s('bot', '', 'awaiting'), s('done', 'bot', 'concluded')], 'bot');
	assert.equal(none.rows, false);
	assert.equal(none.kept.size, 0);
	assert.equal(subagentTree([s('a', 'b', 'ongoing'), s('b', 'a', 'ongoing')], 'a').live, 2);
	assert.equal(subagentTree([s('bot', '', 'awaiting')], '').rows, false);
});
