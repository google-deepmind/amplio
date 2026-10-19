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

import { runMenu } from '$lib/runMenu.svelte';
import { viewport } from '$lib/viewport.svelte';

/**
 * Edge-swipe to open the run drawer, as an action: `use:swipeMenu`. Only a drag
 * that STARTS near the left edge counts, so it cannot fight horizontal
 * scrollers in the middle of the view, and it bails the moment the gesture
 * looks vertical, so it cannot fight the page scroll.
 */
export function swipeMenu(node: HTMLElement) {
	const EDGE = 28; // px from the left edge where the swipe may start
	const DIST = 70; // px of horizontal travel that commits the gesture
	let x0 = 0;
	let y0 = 0;
	let tracking = false;

	const start = (e: TouchEvent) => {
		const t = e.touches[0];
		tracking = viewport.isPhone && !runMenu.open && t.clientX <= EDGE;
		x0 = t.clientX;
		y0 = t.clientY;
	};
	const move = (e: TouchEvent) => {
		if (!tracking) return;
		const t = e.touches[0];
		const dx = t.clientX - x0;
		const dy = t.clientY - y0;
		if (Math.abs(dy) > Math.abs(dx)) {
			tracking = false; // a scroll, not a swipe
			return;
		}
		if (dx > DIST) {
			tracking = false;
			runMenu.open = true;
		}
	};
	const end = () => (tracking = false);

	node.addEventListener('touchstart', start, { passive: true });
	node.addEventListener('touchmove', move, { passive: true });
	node.addEventListener('touchend', end, { passive: true });
	node.addEventListener('touchcancel', end, { passive: true });
	return {
		destroy() {
			node.removeEventListener('touchstart', start);
			node.removeEventListener('touchmove', move);
			node.removeEventListener('touchend', end);
			node.removeEventListener('touchcancel', end);
		}
	};
}
