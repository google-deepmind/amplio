/*
 Copyright 2026 Google LLC

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

// RetryingLoad runs a load until it succeeds once. After a failure it tries
// again after a delay that doubles from firstMs up to maxMs, and at once when
// load() is called again (say, when the server is reachable again).
export class RetryingLoad {
  #done = false;
  #pending: Promise<void> | null = null;
  #timer: ReturnType<typeof setTimeout> | undefined;
  #failures = 0;
  readonly #attempt: () => Promise<void>;
  readonly #firstMs: number;
  readonly #maxMs: number;

  constructor(attempt: () => Promise<void>, firstMs = 1_000, maxMs = 30_000) {
    this.#attempt = attempt;
    this.#firstMs = firstMs;
    this.#maxMs = maxMs;
  }

  get done(): boolean {
    return this.#done;
  }

  // load resolves when this try ends, whether it succeeded or not.
  load(): Promise<void> {
    if (this.#done) return Promise.resolve();
    if (this.#pending) return this.#pending;
    clearTimeout(this.#timer);
    this.#timer = undefined;
    this.#pending = this.#attempt()
      .then(
        () => {
          this.#done = true;
        },
        () => {
          const delay = Math.min(this.#firstMs * 2 ** this.#failures, this.#maxMs);
          this.#failures++;
          this.#timer = setTimeout(() => void this.load(), delay);
        },
      )
      .finally(() => {
        this.#pending = null;
      });
    return this.#pending;
  }
}
