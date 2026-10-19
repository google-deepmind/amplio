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
	// replaces the app banner (hidden here by TopBanner) and absorbs what was a
	// second header row. Menu, title, grade, status, run actions — one line.
	// No back button: the drawer reaches every destination (an edge swipe also
	// opens it), and the row is too valuable to spend on a third control.
	import type { Snippet } from 'svelte';
	import { ListIcon } from 'phosphor-svelte';
	import StatusBadge from './StatusBadge.svelte';
	import { runMenu } from '$lib/runMenu.svelte';

	let {
		title,
		status,
		trailing
	}: { title: string; status?: string; trailing?: Snippet } = $props();
</script>

<header class="bar">
	<button
		class="menu"
		aria-label="Run sections"
		aria-expanded={runMenu.open}
		onclick={() => (runMenu.open = !runMenu.open)}
	>
		<ListIcon size={20} weight="bold" />
	</button>
	<span class="title">{title}</span>
	{#if trailing}<span class="trailing">{@render trailing()}</span>{/if}
	{#if status}<StatusBadge {status} />{/if}
</header>

<style>
	/* Same inset, gap, icon and type scale as the app banner on the dashboard
	   (TopBanner's phone block): this bar REPLACES that one inside a run, so a
	   different rhythm reads as a different app. Only the height differs, and
	   only because this bar carries no status chips. */
	.bar {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		padding: 0.35rem 0.6rem;
		border-bottom: 1px solid var(--border);
		background: var(--bg-elev);
	}
	/* 44px of tap from an overlay, not from the box: a 44px-tall button would
	   set the height of the only bar on the screen. */
	.menu {
		position: relative;
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-height: 0;
		padding: 0.2rem;
		background: none;
		border: none;
		color: var(--text);
	}
	.menu::after {
		content: '';
		position: absolute;
		inset: -11px -8px;
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
	/* The run card, stripped to its controls (see RunCard's phone rules). */
	.trailing {
		display: inline-flex;
		align-items: center;
		flex-shrink: 0;
	}
</style>
