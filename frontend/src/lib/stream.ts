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

import { serverHealth } from './serverHealth.svelte';
import { sysstat } from './sysstat.svelte';
import type { RunEvent } from './types';

// openStream subscribes to the SSE liveness channel for one run (or all runs
// when runID is null, for the dashboard). Returns a disposer.
//
// The stream is an open read; EventSource sends the same-origin auth cookie
// automatically, so no token is needed in the URL. Invalidation events tell the
// caller WHAT to refetch; stream_chunk carries ephemeral token deltas. The
// browser auto-reconnects, and the server primes each (re)connection with a
// refetch_all.
export function openStream(runID: string | null, onEvent: (ev: RunEvent) => void): () => void {
	const url = runID ? `/api/runs/${runID}/stream` : '/api/stream';
	let es = connect();

	function connect(): EventSource {
		const src = new EventSource(url);
		src.onopen = () => serverHealth.markOk();
		src.onerror = () => {
			// CONNECTING is transient (serverHealth promotes after a grace period);
			// CLOSED is a hard failure.
			if (src.readyState === EventSource.CLOSED) serverHealth.markDown();
			else serverHealth.markReconnecting();
		};
		src.onmessage = (e: MessageEvent) => {
			try {
				const ev = JSON.parse(e.data) as RunEvent;
				// Global signals go straight to their stores; run-scoped events still
				// flow to onEvent.
				if (ev.kind === 'sysstat' && ev.sysstat) {
					sysstat.update(ev.sysstat);
					return;
				}
				onEvent(ev);
			} catch {
				// ignore malformed frame
			}
		};
		return src;
	}

	// The browser's auto-reconnect gives up at CLOSED, and mobile Safari lands
	// there routinely by tearing down a backgrounded tab's sockets: the page
	// comes back looking alive while showing a frozen run. Returning to the
	// foreground is the cheapest moment to check. Redial on anything that isn't
	// OPEN — a woken tab is often mid-backoff in CONNECTING — and let the
	// server's refetch_all prime repair whatever was missed.
	const onVisible = () => {
		if (document.visibilityState !== 'visible') return;
		if (es.readyState === EventSource.OPEN) return;
		es.close(); // no-op if already closed; prevents two live sockets per stream
		es = connect();
	};
	document.addEventListener('visibilitychange', onVisible);

	return () => {
		document.removeEventListener('visibilitychange', onVisible);
		es.close();
	};
}
