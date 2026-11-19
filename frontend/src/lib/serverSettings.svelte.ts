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

// The server's settings, from the open /api/settings, fetched once per page
// load (retried until it succeeds): its time zone (timestamps render in the
// zone the agents see, see lib/time.ts, not the browser's), its clock, and the
// defaults the verification and loop forms fill in.

import { api } from "$lib/api";
import type { ServerSettings as Settings } from "$lib/types";
import { clockOffset } from "$lib/loopSchedule";
import { RetryingLoad } from "$lib/retryingLoad";

// acceptedZone returns zone if this browser's Intl knows it, else undefined
// (the browser's own zone).
function acceptedZone(zone: string | null): string | undefined {
  if (!zone) return undefined;
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: zone });
    return zone;
  } catch {
    return undefined;
  }
}

class ServerSettings {
  // The IANA zone the server reports; null until known, and if the server
  // can't name its zone.
  timezone = $state<string | null>(null);
  // The zone timestamps render in: the server's, or undefined (the browser's)
  // until it is known, if the server can't name it, or if this browser rejects
  // the name.
  displayZone = $derived(acceptedZone(this.timezone));
  // The settings arrived.
  loaded = $state(false);
  // [verify] and [loop] defaults; the built-in ones until the server answers.
  verify = $state<NonNullable<Settings["verify"]>>({ max_rounds: 3, model: "" });
  loop = $state<NonNullable<Settings["loop"]>>({ expiry: "168h", min_interval_seconds: 60 });
  // The server's clock minus the browser's, from the settings' `now` (half the
  // round trip in), so a loop's "send now" is the server's now.
  #offset = 0;
  #loader = new RetryingLoad(() => {
    const sent = Date.now();
    return api.getSettings().then((s) => {
      this.#offset = clockOffset(s.now, sent, Date.now());
      this.timezone = s.timezone || null;
      if (s.verify) this.verify = s.verify;
      if (s.loop) this.loop = s.loop;
      this.loaded = true;
    });
  });

  // now is the server's clock, as well as the browser can tell (its own until
  // the settings arrive).
  now(): number {
    return Date.now() + this.#offset;
  }

  // load fetches the settings if they haven't arrived yet; a failure is
  // retried with backoff, and by the next load().
  load(): Promise<void> {
    return this.#loader.load();
  }
}

export const serverSettings = new ServerSettings();
