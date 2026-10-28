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

import { pushState } from '$app/navigation';

/** The drawer dialog's id, for the ☰ button's aria-controls. */
export const DRAWER_ID = 'nav-drawer';

// One phone drawer exists at a time (the dashboard's or the run's), so one
// flag serves both. Opening pushes a shallow history entry, so the system Back
// (Android button, iOS swipe) closes the drawer instead of leaving the page;
// links inside the drawer replace that entry (data-sveltekit-replacestate on
// the dialog), so a later Back never lands on it. The open flag is module
// state rather than page.state; page.state would also work now that ?token=
// is exchanged before the router starts (hooks.client.ts).
let open = $state(false);
let entry = false; // our shallow entry is the current one

export const navDrawer = {
	get open() {
		return open;
	},
	show() {
		if (open) return;
		pushState('', {});
		entry = true;
		open = true;
	},
	hide() {
		if (!open) return;
		open = false;
		if (entry) {
			entry = false;
			history.back();
		}
	},
	/** Back popped the entry, or a navigation replaced it. */
	dropped() {
		open = false;
		entry = false;
	}
};
