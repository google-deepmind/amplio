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

import { viewport } from './viewport.svelte';

// A deliberate reading drag runs at ~0.1-0.3 px/ms, a flick at 1-3 px/ms.
const MIN_SPEED = 0.5; // px/ms over the last WINDOW_MS
const MIN_TRAVEL = 32; // px of net downward finger travel since touchstart
const WINDOW_MS = 100;

type Sample = { y: number; t: number };

function editableFocused(): HTMLElement | null {
	const el = document.activeElement as HTMLElement | null;
	if (!el) return null;
	if (el.isContentEditable || el.tagName === 'TEXTAREA') return el;
	if (
		el.tagName === 'INPUT' &&
		/^(text|search|url|email|tel|password|number)$/.test((el as HTMLInputElement).type)
	) {
		return el;
	}
	return null;
}

// A touch that starts on an element with touch-action: none (a scrubber, a
// slider) drives that control, not the scroll, so it is not a dismiss gesture.
function startsOnNonScrollingControl(target: EventTarget | null, root: HTMLElement): boolean {
	for (let el = target as Element | null; el && el !== root; el = el.parentElement) {
		if (getComputedStyle(el).touchAction === 'none') return true;
	}
	return false;
}

/**
 * Phone only: a fast finger-down drag on `node` (the messaging-app gesture of
 * pushing the keyboard away, revealing older messages) blurs the focused text
 * field, which closes the virtual keyboard. Driven by touch events alone, so
 * programmatic scrolls (pin-to-bottom, scrollIntoView, momentum) never count.
 */
export function dismissKeyboardOnScroll(node: HTMLElement) {
	let start: { x: number; y: number } | null = null;
	let samples: Sample[] = [];

	const check = (x: number, y: number, t: number) => {
		if (!start) return;
		samples.push({ y, t });
		while (samples.length > 2 && t - samples[1].t >= WINDOW_MS) samples.shift();
		const dy = y - start.y;
		if (dy < MIN_TRAVEL || Math.abs(x - start.x) >= dy) return;
		const first = samples[0];
		const dt = t - first.t;
		if (dt <= 0 || (y - first.y) / dt < MIN_SPEED) return;
		start = null; // once per gesture
		editableFocused()?.blur();
	};

	const onStart = (e: TouchEvent) => {
		start = null;
		if (e.touches.length !== 1 || !viewport.isPhone || !editableFocused()) return;
		if (startsOnNonScrollingControl(e.target, node)) return;
		const p = e.touches[0];
		start = { x: p.clientX, y: p.clientY };
		samples = [{ y: p.clientY, t: e.timeStamp }];
	};
	const onMove = (e: TouchEvent) => {
		if (e.touches.length !== 1) start = null;
		const p = e.touches[0];
		if (p) check(p.clientX, p.clientY, e.timeStamp);
	};
	// The lift position can be past the last touchmove on a quick flick.
	const onEnd = (e: TouchEvent) => {
		const p = e.changedTouches[0];
		if (p && e.touches.length === 0) check(p.clientX, p.clientY, e.timeStamp);
		start = null;
	};
	const onCancel = () => (start = null);

	const opts = { passive: true };
	node.addEventListener('touchstart', onStart, opts);
	node.addEventListener('touchmove', onMove, opts);
	node.addEventListener('touchend', onEnd, opts);
	node.addEventListener('touchcancel', onCancel, opts);
	return {
		destroy() {
			node.removeEventListener('touchstart', onStart);
			node.removeEventListener('touchmove', onMove);
			node.removeEventListener('touchend', onEnd);
			node.removeEventListener('touchcancel', onCancel);
		}
	};
}
