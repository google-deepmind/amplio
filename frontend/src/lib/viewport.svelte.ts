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

import { MediaQuery } from 'svelte/reactivity';

// Form-factor decisions from one set of numbers, kept textually identical to
// the CSS breakpoints. For decisions CSS cannot express (different markup, a
// click routed elsewhere); pure styling stays in `@media` blocks.
//
// Every query is built HERE, at module scope, or at the top level of a
// component script — never inside an `{#if}`, a `$derived` or an `$effect`.
// A signal created during a reaction is treated as owned by it, so that
// reaction never subscribes and never re-runs when the query flips.

// A prerender pass evaluates modules with no window, and MediaQuery calls
// matchMedia in its constructor. Desktop is the safer fallback: it degrades on
// a phone for the frame before hydration; the reverse does not.
const SSR_WIDTH = 1400;

// Just above the widest phone in portrait, well below any laptop.
const PHONE_MAX = 640;
// Tablets and narrow split-screen windows.
const TABLET_MAX = 1024;

type Query = { readonly current: boolean };

function query(q: string, fallback: boolean): Query {
	if (typeof window === 'undefined' || !window.matchMedia) return { current: fallback };
	return new MediaQuery(q, fallback);
}

const phone = query(`max-width: ${PHONE_MAX}px`, SSR_WIDTH <= PHONE_MAX);
const tablet = query(
	`(min-width: ${PHONE_MAX + 1}px) and (max-width: ${TABLET_MAX}px)`,
	SSR_WIDTH > PHONE_MAX && SSR_WIDTH <= TABLET_MAX
);

export const viewport = {
	/** Phone-sized: bottom tab bar, single column, no side panels. */
	get isPhone() {
		return phone.current;
	},
	/** Between phone and desktop: the desktop chrome fits, wide splits do not. */
	get isTablet() {
		return tablet.current;
	}
};

/**
 * Reactive `(min-width: <px>)` for a threshold that belongs to one view. Call
 * it at the top level of the component script and read `.current` where you
 * need it; creating it inside a reaction breaks the subscription (above).
 */
export function atLeast(px: number): Query {
	return query(`min-width: ${px}px`, SSR_WIDTH >= px);
}
