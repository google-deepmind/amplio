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

// Web auth state. The server gates writes on a token presented as an HttpOnly
// cookie (set once via the magic-link exchange) or a Bearer header (CLI). Reads
// are open, so a token-less shared URL resolves to a readonly view.
//
// consumeURLToken() runs from the client `init` hook (hooks.client.ts), before
// the router starts: if the URL carries ?token= (the link the owner opened from
// their terminal), it strips the token from the address bar and POSTs it to
// /api/auth/login so the server sets the cookie. The router therefore never sees
// the token (it cannot resurface on a later goto), and no page's first API call
// can race the cookie. A copied (token-less) URL shared with a teammate lands
// them in readonly. `authed` drives the UI (hide write controls); the server
// enforces it regardless.
class Auth {
	authed = $state(false);
	user = $state(''); // server owner's username, for the readonly banner
	ready = $state(false); // false until /api/auth has answered (avoid UI flash)

	async consumeURLToken() {
		const url = new URL(window.location.href);
		const token = url.searchParams.get('token');
		if (!token) return;
		url.searchParams.delete('token');
		// The router has not started, so nothing to conflict with; the prototype
		// call skips SvelteKit's dev-only warning wrapper on history.replaceState.
		History.prototype.replaceState.call(history, history.state, '', url);
		try {
			await fetch(`/api/auth/login?token=${encodeURIComponent(token)}`, {
				method: 'POST',
				signal: AbortSignal.timeout(10000),
			});
		} catch {
			// network error or timeout — start anyway; we'll resolve to readonly
		}
	}

	async init() {
		if (typeof window === 'undefined') return;
		await this.refresh();
	}

	async refresh() {
		try {
			const res = await fetch('/api/auth');
			if (res.ok) {
				const d = (await res.json()) as { authed?: boolean; user?: string };
				this.authed = !!d.authed;
				this.user = d.user ?? '';
			}
		} catch {
			// leave defaults (readonly)
		}
		this.ready = true;
	}
}

export const auth = new Auth();
