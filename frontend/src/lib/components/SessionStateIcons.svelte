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
	// A session's state as small icons, each a link to it in the Inspector,
	// shown only while active: a seal while its verification is in force, a
	// repeat while it has loops (lib/sessionState words them). Nothing for a
	// session with neither.
	import { RepeatIcon, SealCheckIcon } from 'phosphor-svelte';
	import { serverSettings } from '$lib/serverSettings.svelte';
	import { formatAhead } from '$lib/loopSchedule';
	import { activeLoops, activeVerify, loopsLabel, verifyLabel } from '$lib/sessionState';
	import type { SessionDTO } from '$lib/types';

	let { runId, session }: { runId: string; session: SessionDTO } = $props();

	const verify = $derived(activeVerify(session));
	const loops = $derived(activeLoops(session));
	const verifyText = $derived(verify ? verifyLabel(verify) : '');
	const loopsText = $derived.by(() => {
		const [zone, now] = [serverSettings.displayZone, serverSettings.now()];
		return loops ? loopsLabel(loops, (ms) => formatAhead(ms, zone, now)) : '';
	});
	const href = (section: string) =>
		`/runs/${runId}/inspector?session=${encodeURIComponent(session.session_id)}#${section}`;
</script>

{#if verify || loops}
	<span class="icons">
		{#if verify}
			<a class="icon verify" href={href('verification')} title={verifyText} aria-label={verifyText}>
				<SealCheckIcon size={14} weight="bold" />
			</a>
		{/if}
		{#if loops}
			<a class="icon" href={href('loops')} title={loopsText} aria-label={loopsText}>
				<RepeatIcon size={14} weight="bold" />
			</a>
		{/if}
	</span>
{/if}

<style>
	.icons {
		display: inline-flex;
		align-items: center;
		gap: 0.15rem;
		flex-shrink: 0;
	}
	.icon {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		padding: 0.2rem;
		border-radius: var(--radius-sm);
		color: var(--text);
	}
	.icon.verify {
		color: var(--accent);
	}
	.icon:hover {
		background: var(--bg-elev2);
		text-decoration: none;
	}
	@media (max-width: 640px) {
		.icon {
			min-width: 32px;
			min-height: 32px;
		}
	}
</style>
