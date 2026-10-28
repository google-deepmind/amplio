/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the 'License');
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an 'AS IS' BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { tick } from 'svelte';
import { pushState } from '$app/navigation';
import { viewport } from './viewport.svelte';

/**
 * Open/close plumbing for a ⋮-style menu whose trigger and menu live in
 * `root`. While open (and only then) it listens for:
 * - a click outside `root`: closes. On a phone the menu is a modal sheet, so
 *   that click is consumed and cannot also activate what lies under the scrim.
 * - Escape: closes, focus back to the trigger (the first button in `root`).
 * - Back (phone): opening pushes a shallow history entry, as Drawer.svelte
 *   does; Back pops it and closes. Any other close pops it with
 *   history.back(), so the next Back navigates normally. Desktop popovers
 *   never touch history.
 */
export function popover(close: () => void) {
	let root: HTMLElement | undefined;
	let entry = false;
	const onClick = (e: MouseEvent) => {
		if (root?.contains(e.target as Node)) return;
		close();
		if (viewport.isPhone) {
			e.preventDefault();
			e.stopPropagation();
		}
	};
	const onKey = (e: KeyboardEvent) => {
		if (e.key !== 'Escape') return;
		const trigger = root?.querySelector('button');
		close();
		// After the sheet unmounts: the trigger is inert while a modal is open.
		tick().then(() => trigger?.focus());
	};
	const onPop = () => {
		if (!entry) return;
		release();
		close();
	};
	function listen(on: boolean) {
		if (on) {
			document.addEventListener('click', onClick, true);
			document.addEventListener('keydown', onKey);
			addEventListener('popstate', onPop);
		} else {
			document.removeEventListener('click', onClick, true);
			document.removeEventListener('keydown', onKey);
			removeEventListener('popstate', onPop);
		}
	}
	/** Forget the history entry without going back. Returns whether one existed. */
	function release() {
		const had = entry;
		entry = false;
		return had;
	}
	return {
		opened(node: HTMLElement) {
			if (root) return;
			root = node;
			listen(true);
			if (!viewport.isPhone) return;
			pushState('', {});
			entry = true;
		},
		/** Stops listening and pops the entry if any; resolves once the Back has landed. */
		closed(): Promise<void> {
			if (!root) return Promise.resolve();
			root = undefined;
			listen(false);
			if (!release()) return Promise.resolve();
			const landed = new Promise<void>((r) => addEventListener('popstate', () => r(), { once: true }));
			history.back();
			return landed;
		},
		release
	};
}

/** Up/Down/Home/End between the enabled menuitems of a role=menu. */
export function menuKeys(e: KeyboardEvent) {
	const items = [
		...(e.currentTarget as HTMLElement).querySelectorAll<HTMLElement>(
			'[role="menuitem"]:not(:disabled)'
		)
	];
	const i = items.indexOf(document.activeElement as HTMLElement);
	const next = {
		ArrowDown: items[(i + 1) % items.length],
		ArrowUp: items[i <= 0 ? items.length - 1 : i - 1],
		Home: items[0],
		End: items[items.length - 1]
	}[e.key];
	if (!next) return;
	e.preventDefault();
	next.focus();
}
