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
import { placeAbove } from './anchoredPlace.ts';

const VIEW = { width: 1280, height: 800 };
const FORM = { width: 480, height: 300 };

test('placeAbove: above the card, at its chip', () => {
	assert.deepEqual(placeAbove(400, { top: 650, bottom: 760 }, FORM, VIEW), { left: 400, top: 344, maxHeight: null });
});

test('placeAbove: kept inside the viewport sideways', () => {
	assert.equal(placeAbove(1000, { top: 650, bottom: 760 }, FORM, VIEW).left, 1280 - 8 - 480);
	assert.equal(placeAbove(2, { top: 650, bottom: 760 }, FORM, VIEW).left, 8);
	assert.equal(placeAbove(100, { top: 650, bottom: 760 }, { width: 2000, height: 300 }, VIEW).left, 8);
});

test('placeAbove: flipped below a card near the top', () => {
	assert.deepEqual(placeAbove(400, { top: 100, bottom: 250 }, FORM, VIEW), { left: 400, top: 256, maxHeight: null });
});

test('placeAbove: on the roomier side, scrolling, when it fits on neither', () => {
	assert.deepEqual(placeAbove(400, { top: 500, bottom: 560 }, { width: 480, height: 600 }, VIEW), {
		left: 400,
		top: 8,
		maxHeight: 486
	});
	assert.deepEqual(placeAbove(400, { top: 200, bottom: 260 }, { width: 480, height: 600 }, VIEW), {
		left: 400,
		top: 266,
		maxHeight: 526
	});
});

test('placeAbove: over the viewport when a tall card leaves no room', () => {
	assert.deepEqual(placeAbove(400, { top: 60, bottom: 780 }, FORM, VIEW), { left: 400, top: 8, maxHeight: 784 });
});
