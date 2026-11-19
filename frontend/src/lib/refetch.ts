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

// Refetching per key (a session) on its events, and dropping responses that a
// newer request has overtaken. Pure, so `npm test` can exercise it in node.

// Coalesces calls per key: 'debounce' runs once the key has been quiet for
// `wait` ms; 'throttle' runs `wait` ms after the first call, whatever follows.
export function coalescer(run: (key: string) => void, wait: number, mode: 'debounce' | 'throttle') {
	const pending = new Map<string, ReturnType<typeof setTimeout>>();
	return {
		schedule(key: string) {
			if (pending.has(key)) {
				if (mode === 'throttle') return;
				clearTimeout(pending.get(key));
			}
			pending.set(
				key,
				setTimeout(() => {
					pending.delete(key);
					run(key);
				}, wait)
			);
		},
		cancel() {
			for (const t of pending.values()) clearTimeout(t);
			pending.clear();
		}
	};
}

// Latest tags requests per key; a request's check passes only while it is the
// newest for its key and no reset (a run switch) came since.
export class Latest {
	#gen = 0;
	#seq = new Map<string, number>();

	reset() {
		this.#gen++;
		this.#seq.clear();
	}

	start(key: string): () => boolean {
		const gen = this.#gen;
		const n = (this.#seq.get(key) ?? 0) + 1;
		this.#seq.set(key, n);
		return () => gen === this.#gen && this.#seq.get(key) === n;
	}
}
