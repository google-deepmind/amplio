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
	// A session's state for a status line, as badges linking to it in the
	// Inspector: its verification (in force, or how its latest version ended)
	// and its loops (lib/sessionState words them). Nothing when neither
	// applies. Phone: the loops badge keeps only its icon and count.
	import { RepeatIcon, SealCheckIcon } from 'phosphor-svelte';
	import { serverSettings } from '$lib/serverSettings.svelte';
	import { formatAhead, formatWhen } from '$lib/loopSchedule';
	import { activeLoops, loopsLabel, shownVerify, verifyBadge, verifyLabel } from '$lib/sessionState';
	import type { SessionDTO } from '$lib/types';

	let { runId, session }: { runId: string; session: SessionDTO } = $props();

	const verify = $derived(shownVerify(session));
	const loops = $derived(activeLoops(session));
	// Short in the badge ("next Tue 12:55"), with the date and zone in its tooltip.
	const loopsText = $derived.by(() => {
		const [zone, now] = [serverSettings.displayZone, serverSettings.now()];
		if (!loops) return { short: '', full: '' };
		return {
			short: loopsLabel(loops, (ms) => formatAhead(ms, zone, now)),
			full: loopsLabel(loops, (ms) => formatWhen(ms, zone, now, { zone: true }))
		};
	});
	const href = (section: string) =>
		`/runs/${runId}/inspector?session=${encodeURIComponent(session.session_id)}#${section}`;
</script>

{#if verify}
	{@const label = `${verifyLabel(verify)}: open in the Inspector`}
	<a class="badge verify {verify.status}" href={href('verification')} title={label} aria-label={label}>
		<SealCheckIcon size={13} weight="bold" />
		<span class="text">{verifyBadge(verify)}</span>
	</a>
{/if}
{#if loops}
	{@const label = `${loopsText.full}: open in the Inspector`}
	<a class="badge" href={href('loops')} title={label} aria-label={label}>
		<RepeatIcon size={13} weight="bold" />
		<span class="text full">{loopsText.short}</span>
		<span class="count">{loops.count}</span>
	</a>
{/if}

<style>
	.badge {
		display: inline-flex;
		align-items: center;
		gap: 0.3rem;
		flex-shrink: 0;
		max-width: 100%;
		padding: 0.05rem 0.5rem;
		border: 1px solid var(--border);
		border-radius: var(--radius-pill);
		color: var(--text);
		font-size: var(--fs-sm);
		white-space: nowrap;
	}
	.badge:hover {
		text-decoration: none;
		border-color: var(--accent-dim);
	}
	.text {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.verify {
		flex-shrink: 1;
		min-width: 0;
	}
	/* As the Inspector colours a verification's state. */
	.verify.on,
	.verify.verifying {
		color: var(--accent);
	}
	.verify.passed {
		color: var(--ok);
	}
	.verify.not_verified {
		color: var(--warn);
	}
	.count {
		display: none;
	}
	@media (max-width: 640px) {
		.full {
			display: none;
		}
		.count {
			display: inline;
		}
		/* A 44px tap target that doesn't grow the line. */
		.badge {
			position: relative;
		}
		.badge::after {
			content: '';
			position: absolute;
			inset: calc((100% - var(--tap)) / 2) 0;
		}
	}
</style>
