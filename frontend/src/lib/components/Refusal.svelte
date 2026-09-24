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

<!--
 Refusal notice: the provider declined this turn (safety classifier, content
 filter, blocked prompt). Shown in place of — or after — the turn's content so a
 refused turn never reads as a silent blank reply. Category and explanation are
 the provider's own, verbatim; the explanation is plain text (never markdown).
-->
<script lang="ts">
	import type { Refusal } from '$lib/types';
	import { ProhibitIcon } from 'phosphor-svelte';

	let { refusal }: { refusal: Refusal } = $props();
</script>

<div class="refusal" role="note">
	<div class="head">
		<ProhibitIcon size={14} weight="bold" />
		<span class="title">Model declined to respond</span>
		{#if refusal.category}<span class="cat">{refusal.category}</span>{/if}
	</div>
	{#if refusal.explanation}
		<div class="why">{refusal.explanation}</div>
	{:else}
		<div class="why dim">The provider gave no reason.</div>
	{/if}
</div>

<style>
	.refusal {
		margin: 0.3rem 0;
		padding: 0.45rem 0.65rem;
		border: 1px solid color-mix(in srgb, var(--err) 40%, transparent);
		border-left: 3px solid var(--err);
		border-radius: var(--radius-sm);
		background: color-mix(in srgb, var(--err) 8%, transparent);
		font-size: var(--fs-sm);
	}
	.head {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		color: var(--err);
	}
	.title {
		font-weight: 600;
	}
	.cat {
		font-family: var(--mono);
		font-size: 0.85em;
		padding: 0 0.35rem;
		border: 1px solid color-mix(in srgb, var(--err) 45%, transparent);
		border-radius: var(--radius-sm);
	}
	.why {
		margin-top: 0.25rem;
		color: var(--text);
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
	.dim {
		color: var(--text-dim);
	}
</style>
