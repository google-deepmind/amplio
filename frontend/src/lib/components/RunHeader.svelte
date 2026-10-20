<!--
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
-->

<script lang="ts">
	import type { RunStore } from '$lib/runContext.svelte';
	import type { RunSummary } from '$lib/types';
	import RunCard from './RunCard.svelte';
	import { viewport } from '$lib/viewport.svelte';

	// Run-page header: renders the unified <RunCard> in non-linkable mode
	// (the page IS this run; clicking the title would go nowhere). The card
	// component needs a RunSummary shape; we synthesize one from RunDetail +
	// sessions instead of round-tripping a separate /summary endpoint.

	let { store }: { store: RunStore } = $props();

	const detail = $derived(store.detail);
	const sessions = $derived(detail?.sessions ?? []);

	// The server's pick (detail.root_session_id), same as the run bar's status.
	const primaryRoot = $derived(
		sessions.find((s) => s.session_id === detail?.root_session_id) ?? null
	);

	const summary = $derived.by((): RunSummary | null => {
		if (!detail) return null;
		const root = primaryRoot;
		return {
			run_id: detail.run_id,
			task: detail.task,
			title: detail.title,
			starred: detail.starred,
			grade: detail.grade,
			report_grade: detail.report_grade,
			archived: detail.archived,
			created_at: detail.created_at,
			workspace: detail.workspace,
			workspace_name: detail.workspace_name,
			workspace_kind: detail.workspace_kind,
			workspace_alias: detail.workspace_alias,
			workspace_numeric_id: detail.workspace_numeric_id,
			cider_url: detail.cider_url,
			llm: detail.llm,
			llm_name: detail.llm_name,
			roots: [], // not consumed by RunCard
			root_session_id: root?.session_id ?? '',
			root_status: root?.status ?? '',
			root_step: root?.current_step ?? 0,
			root_status_changed_at: root?.status_changed_at ?? detail.created_at,
			root_agent_type: root?.agent_type ?? '',
			session_count: sessions.length,
			// The run-page header always reflects a run the operator is viewing (so it
			// gets marked seen); no unread badge on its own card.
			has_updates: false
		};
	});
</script>

{#if summary}
	<!-- On a phone the run layout puts this header inside RunTopBar. -->
	<RunCard run={summary} inBar={viewport.isPhone} onmutated={() => store.refresh()} />
{/if}
