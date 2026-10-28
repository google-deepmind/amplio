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

// iOS Safari ignores `interactive-widget=resizes-content` (app.html): with the
// virtual keyboard open it keeps the layout viewport full height and pans the
// page, sliding the top bar off screen. While a keyboard is open we size the
// app shell to the visible area (`body { height: var(--app-h, 100dvh) }` in
// app.css) and undo the pan. Where the browser already resizes the layout
// viewport (Chrome/Firefox Android) the visual and layout heights agree, so
// this stays a no-op; likewise on desktop.

// Larger than any URL-bar / toolbar delta, smaller than any phone keyboard.
const MIN_KEYBOARD_PX = 120;

function editableFocused(doc: Document): boolean {
	const el = doc.activeElement as HTMLElement | null;
	if (!el) return false;
	if (el.isContentEditable || el.tagName === 'TEXTAREA') return true;
	return (
		el.tagName === 'INPUT' &&
		!/^(button|checkbox|color|file|hidden|image|radio|range|reset|submit)$/.test(
			(el as HTMLInputElement).type
		)
	);
}

/** Starts tracking; returns the cleanup. Pass to `onMount` once, at the app root. */
export function trackKeyboard(): () => void {
	const vv = window.visualViewport;
	if (!vv) return () => {};
	const root = document.documentElement;
	let frame = 0;
	let appliedH = 0;
	let appliedEl: Element | null = null;
	let isOpen = false;

	const apply = () => {
		frame = 0;
		// clientHeight is the layout viewport: it shrinks under resizes-content
		// (Android), stays full height on iOS. Pinch zoom also shrinks vv.height.
		const covered = root.clientHeight - vv.height;
		const open =
			covered > MIN_KEYBOARD_PX && Math.abs(vv.scale - 1) < 0.01 && editableFocused(document);
		if (open) {
			const h = Math.round(vv.height);
			root.style.setProperty('--app-h', `${h}px`);
			// iOS scrolled the field into view against the full-height layout;
			// redo it inside the shorter shell. Only when the height or the
			// focused field changes, so the user can still scroll away while
			// typing. A field change matters because the pan undone below may
			// have been Safari bringing the new field into view.
			if (h !== appliedH || document.activeElement !== appliedEl)
				(document.activeElement as HTMLElement).scrollIntoView({ block: 'nearest' });
			appliedH = h;
			appliedEl = document.activeElement;
			if (window.scrollY !== 0 || vv.offsetTop !== 0) window.scrollTo(0, 0);
		} else {
			if (isOpen) root.style.removeProperty('--app-h');
			appliedH = 0;
			appliedEl = null;
		}
		isOpen = open;
	};
	const schedule = () => {
		if (!frame) frame = requestAnimationFrame(apply);
	};

	vv.addEventListener('resize', schedule);
	vv.addEventListener('scroll', schedule);
	// Moving between fields with the keyboard up fires no viewport resize.
	document.addEventListener('focusin', schedule);
	return () => {
		vv.removeEventListener('resize', schedule);
		vv.removeEventListener('scroll', schedule);
		document.removeEventListener('focusin', schedule);
		if (frame) cancelAnimationFrame(frame);
		root.style.removeProperty('--app-h');
		isOpen = false;
	};
}
