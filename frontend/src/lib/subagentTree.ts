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


// The chat page's sub-agent tree: the subtree below its chatbot worth showing.
// Pure, so `npm test` can exercise it in node.

import { activeLoops } from './sessionState.ts';
import type { SessionDTO } from './types.ts';

type Session = Pick<SessionDTO, 'session_id' | 'parent_id' | 'status' | 'interactive' | 'verify' | 'loops'>;

export interface SubagentTree<S extends Session = SessionDTO> {
	kept: Map<string, S[]>; // the rows under each parent, in spawn order
	live: number; // rows that are running
	looping: number; // rows idle between loop ticks
	depth: number; // levels below the root
	rows: boolean; // anything to show
}

const ACTIVE = new Set(['ongoing', 'awaiting']);

// The live subtree below rootId, not just its direct children: agents spawn
// agents that spawn agents, so a one-level list reports three sub-agents while
// a dozen are running.
//
// A node is kept if it is active itself OR leads to one. The second case is a
// crashed parent whose children were left running — conclude() cascade-cancels
// its children, recordFailure() deliberately does not — which is exactly the
// situation worth seeing, so those rows render normally and their own status
// pill says "crashed".
//
// A sub-agent with active loops is kept too, though idle between ticks: it
// will run again, and its loop icon says why it is listed. It counts in
// `looping`, not in `live`: nothing is running.
export function subagentTree<S extends Session>(sessions: readonly S[], rootId: string): SubagentTree<S> {
	const kids = new Map<string, S[]>();
	for (const s of sessions) {
		if (!s.parent_id) continue;
		const list = kids.get(s.parent_id);
		if (list) list.push(s);
		else kids.set(s.parent_id, [s]);
	}
	const kept = new Map<string, S[]>();
	const seen = new Set<string>(); // a malformed parent cycle must not hang the UI
	let live = 0;
	let looping = 0;
	let depth = 0;
	const walk = (id: string, level: number): boolean => {
		if (seen.has(id)) return false;
		seen.add(id);
		const out: S[] = [];
		for (const c of kids.get(id) ?? []) {
			const leadsToShown = walk(c.session_id, level + 1);
			const self = ACTIVE.has(c.status);
			const loops = !self && activeLoops(c) !== null;
			if (self) live++;
			if (loops) looping++;
			if (self || loops || leadsToShown) {
				out.push(c);
				depth = Math.max(depth, level);
			}
		}
		if (out.length) kept.set(id, out);
		return out.length > 0;
	};
	if (rootId) walk(rootId, 1);
	return { kept, live, looping, depth, rows: kept.size > 0 };
}
