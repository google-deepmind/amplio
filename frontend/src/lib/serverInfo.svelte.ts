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

// Facts about the SERVER that the UI needs before deciding what to offer —
// today just its platform, which gates the process view (it reads /proc, so it
// only exists on Linux). Deliberately NOT the browser's platform: the agents
// run where the server runs, and a Mac browsing a Linux server has the feature.
//
// Fetched at most once per page load from the open /api/about, then cached: a
// server cannot change its OS while it is running.

import { api } from "$lib/api";

class ServerInfo {
  platform = $state<string | null>(null); // null = not asked yet / unreachable
  #pending: Promise<void> | null = null;

  // True only once we KNOW it is Linux. Callers hide a Linux-only entry while
  // the answer is pending rather than showing one that would vanish under the
  // cursor a moment later.
  get isLinux(): boolean {
    return this.platform === "linux";
  }

  load(): Promise<void> {
    if (this.platform !== null) return Promise.resolve();
    this.#pending ??= api
      .getAbout()
      .then((info) => {
        this.platform = info.platform ?? "";
      })
      .catch(() => {
        // An unreachable server is the banner's problem, not ours; leaving
        // platform null just means the gated entry stays hidden.
        this.#pending = null;
      });
    return this.#pending;
  }
}

export const serverInfo = new ServerInfo();
