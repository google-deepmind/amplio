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

import { onDestroy } from 'svelte';
import { beforeNavigate, pushState } from '$app/navigation';
import { viewport } from '$lib/viewport.svelte';

// Phone sheets that Back closes: the new-run form and the pickers inside it.
// Each open layer owns one shallow history entry, stamped with its depth, so
// nested layers pop in order (picker, then form) and a Back closes exactly the
// layers above the entry it lands on. The depth is read from history.state;
// an entry this module did not push has no depth and reads as 0.
type Layer = { close: () => void };
const stack: Layer[] = [];

function landedDepth(): number {
	const s = history.state as Record<string, unknown> | null;
	const st = (s?.['sveltekit:states'] ?? s) as { sheetDepth?: number } | null;
	return st?.sheetDepth ?? 0;
}

let listening = false;
function listen() {
	if (listening) return;
	listening = true;
	window.addEventListener('popstate', () => {
		const d = landedDepth();
		while (stack.length > d) stack.pop()!.close();
	});
}

/**
 * While isOpen() on phone, hold a history entry that Back pops (calling close).
 * Closing any other way pops it, and the entries of layers opened above it.
 * A navigation drops the entry without popping (popping would undo it).
 * Call during component init.
 */
export function closeOnBack(isOpen: () => boolean, close: () => void) {
	let layer: Layer | null = null;
	const release = (pop: boolean) => {
		const i = layer ? stack.indexOf(layer) : -1;
		layer = null;
		if (i < 0) return;
		const removed = stack.splice(i);
		for (const l of removed.slice(1)) l.close();
		if (pop) history.go(-removed.length);
	};
	$effect(() => {
		if (isOpen()) {
			if (viewport.isPhone && !layer) {
				listen();
				layer = { close };
				stack.push(layer);
				pushState('', { sheetDepth: stack.length });
			}
		} else if (layer) release(true);
	});
	beforeNavigate(() => release(false));
	onDestroy(() => release(true));
}
