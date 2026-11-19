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

// Sending what a composer composed, and the new-run form's root loop. The API
// is passed in, so `npm test` can exercise the requests in node.

import { buildSpec, type LoopFields } from './loopSchedule.ts';
import { verifyRequest, type VerifyFields, type VerifyRequest } from './composerOptions.ts';
import type { LoopDTO, LoopSchedule } from './types.ts';

// The calls these need (lib/api's).
export interface SendApi {
	sendMessage(id: string, sid: string, content: string, verify?: VerifyRequest): Promise<unknown>;
	addLoop(id: string, sid: string, prompt: string, schedule: LoopSchedule, verify?: VerifyRequest): Promise<LoopDTO>;
	getRun(id: string): Promise<{ root_session_id: string }>;
}

// A message's options. With both, the message repeats and each tick is
// verified.
export interface MessageOptions {
	verify: VerifyFields | null;
	loop: LoopFields | null;
}

export type Sent = { kind: 'message' } | { kind: 'loop'; loop: LoopDTO };

// deliver sends content to a session. A repeat creates a loop whose prompt is
// the message (with send now, its first tick is the message) instead of
// posting it; verification rides on the message, or, with a repeat, on the
// loop, which starts it anew with each tick. now is the server's clock.
export async function deliver(
	api: SendApi,
	runId: string,
	sid: string,
	content: string,
	opts: MessageOptions,
	now: number
): Promise<Sent> {
	const verify = opts.verify ? verifyRequest(opts.verify) : undefined;
	if (opts.loop) {
		const spec = buildSpec(opts.loop, now);
		return { kind: 'loop', loop: await api.addLoop(runId, sid, content.trim(), { kind: 'rrule', spec }, verify) };
	}
	await api.sendMessage(runId, sid, content, verify);
	return { kind: 'message' };
}

// addRootLoop sets a loop on a new run's root agent (the new-run form's loop has
// no send now, so its first tick is one interval after the start), verifying
// each tick with verify if it is set. The root's session row is written by its
// goroutine just after POST /api/runs returns, so this waits (tries × every
// ms) for the run to name it.
export async function addRootLoop(
	api: SendApi,
	runId: string,
	prompt: string,
	loop: LoopFields,
	verify: VerifyFields | null,
	now: () => number,
	wait = { tries: 50, every: 200 }
): Promise<LoopDTO> {
	const v = verify ? verifyRequest(verify) : undefined;
	for (let i = 1; ; i++) {
		const { root_session_id: sid } = await api.getRun(runId);
		if (sid) return api.addLoop(runId, sid, prompt.trim(), { kind: 'rrule', spec: buildSpec(loop, now()) }, v);
		if (i >= wait.tries) throw new Error(`its root agent didn't start within ${(wait.tries * wait.every) / 1000} s`);
		await new Promise((r) => setTimeout(r, wait.every));
	}
}
