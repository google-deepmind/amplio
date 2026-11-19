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

// Where a popover anchored to a composer card goes. Pure, so `npm test` can
// exercise it in node.

export interface Placement {
	left: number;
	top: number;
	maxHeight: number | null; // set when it fits on neither side: it scrolls
}

// Below this much room on either side, the popover covers the viewport instead.
const MIN_ROOM = 160;

// placeAbove puts a popover of `size` above `band` (the card; the composer sits
// low on the page), else below it, else on the roomier side, scrolling. Its left
// edge is `anchorLeft` (the chip it opened from), moved in to keep it `margin`
// px inside the viewport. Coordinates are the viewport's.
export function placeAbove(
	anchorLeft: number,
	band: { top: number; bottom: number },
	size: { width: number; height: number },
	view: { width: number; height: number },
	gap = 6,
	margin = 8
): Placement {
	const left = Math.max(margin, Math.min(anchorLeft, view.width - margin - size.width));
	const above = band.top - gap - margin;
	const below = view.height - margin - band.bottom - gap;
	if (size.height <= above) return { left, top: band.top - gap - size.height, maxHeight: null };
	if (size.height <= below) return { left, top: band.bottom + gap, maxHeight: null };
	if (Math.max(above, below) < MIN_ROOM) return { left, top: margin, maxHeight: view.height - 2 * margin };
	return above >= below ? { left, top: margin, maxHeight: above } : { left, top: band.bottom + gap, maxHeight: below };
}
