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

// Process-lifetime cache for the briefing library, prefetched at page load so
// the picker is part of the composer from the first frame instead of appearing
// a moment after it. Mirrors modelMenu: the library changes only when the
// operator edits <data-dir>/briefings, which needs a server restart to be seen.
import { api } from "./api";
import type { BriefingInfo } from "./types";

let cache: BriefingInfo[] | null = null;
let inflight: Promise<BriefingInfo[]> | null = null;

// cachedBriefings returns the prefetched library if available, else null.
export function cachedBriefings(): BriefingInfo[] | null {
  return cache;
}

// loadBriefings resolves the library, memoizing. A failure clears the in-flight
// so a later call retries.
export function loadBriefings(): Promise<BriefingInfo[]> {
  if (cache) return Promise.resolve(cache);
  if (!inflight) {
    inflight = api
      .listBriefings()
      .then((b) => {
        cache = b;
        return b;
      })
      .catch((e) => {
        inflight = null;
        throw e;
      });
  }
  return inflight;
}

// prefetchBriefings warms the cache; safe to call early and to ignore errors.
export function prefetchBriefings(): void {
  loadBriefings().catch(() => {});
}
