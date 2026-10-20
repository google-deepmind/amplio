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
	// Phone-only bar for the run shell, and the ONLY chrome above the view: it
	// replaces the app banner (TopBanner renders nothing here on a phone).
	// Menu, title, grade, status, run actions — one line. No back button: the
	// drawer reaches every destination, and the row is too valuable to spend on
	// a third control.
	import type { Snippet } from 'svelte';
	import { EyeIcon } from 'phosphor-svelte';
	import StatusBadge from './StatusBadge.svelte';
	import MenuButton from './MenuButton.svelte';
	import { auth } from '$lib/auth.svelte';

	let {
		title,
		status,
		trailing
	}: { title: string; status?: string; trailing?: Snippet } = $props();
</script>

<header class="bar">
	<MenuButton />
	<span class="title">{title}</span>
	{#if auth.ready && !auth.authed}
		<!-- Why there is no grade or ⋮: they are hidden for read-only viewers. -->
		<span class="readonly" title="Read-only shared view" aria-label="Read-only view" role="img">
			<EyeIcon size={18} weight="bold" />
		</span>
	{/if}
	{#if trailing}<span class="trailing">{@render trailing()}</span>{/if}
	{#if status}<StatusBadge {status} />{/if}
</header>

<style>
	/* Same height, ☰ and title position as TopBanner's phone bar (spec §2.2):
	   this bar replaces that one inside a run. */
	.bar {
		display: flex;
		align-items: center;
		gap: var(--sp-1);
		height: var(--bar-h);
		flex-shrink: 0;
		padding: 0 var(--sp-2) 0 var(--sp-1);
		border-bottom: 1px solid var(--border);
		background: var(--bg-elev);
	}
	.title {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: var(--fs-lg);
		font-weight: 600;
		letter-spacing: 0.01em;
	}
	.readonly {
		display: inline-flex;
		padding: 0 var(--sp-1);
		color: var(--text-dim);
	}
	/* The run card, stripped to its controls (see RunCard's phone rules). */
	.trailing {
		display: inline-flex;
		align-items: center;
		flex-shrink: 0;
	}
</style>
