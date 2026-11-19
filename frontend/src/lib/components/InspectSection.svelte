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
	// One of the inspector's sections (Verification, Loops, Processes): a header
	// with its icon, title, an optional badge and actions, then the content.
	import type { Component, Snippet } from 'svelte';
	import type { IconComponentProps } from 'phosphor-svelte';

	let {
		title,
		icon: Icon,
		badge,
		actions,
		children
	}: {
		title: string;
		icon: Component<IconComponentProps>;
		badge?: Snippet;
		actions?: Snippet;
		children: Snippet;
	} = $props();
</script>

<section class="sec" aria-label={title}>
	<header class="sec-head">
		<Icon size={16} weight="bold" />
		<h3>{title}</h3>
		{#if badge}{@render badge()}{/if}
		{#if actions}<span class="actions">{@render actions()}</span>{/if}
	</header>
	{@render children()}
</section>

<style>
	.sec {
		background: var(--bg-elev);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		padding: 0.75rem 1rem;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
		min-width: 0;
	}
	.sec-head {
		display: flex;
		align-items: center;
		gap: 0.45rem;
		flex-wrap: wrap;
		color: var(--text-strong);
	}
	h3 {
		margin: 0;
		font-size: var(--fs-lg);
		font-weight: 600;
	}
	.actions {
		margin-left: auto;
		display: inline-flex;
		gap: 0.4rem;
	}
	@media (max-width: 640px) {
		.sec {
			padding: var(--card-pad-y) var(--card-pad-x);
		}
	}
</style>
