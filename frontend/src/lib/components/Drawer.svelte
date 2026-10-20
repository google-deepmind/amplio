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
	// The phone navigation drawer, one for the whole app: the dashboard banner
	// and the run shell both open this, the run adding its own section on top.
	// A modal <dialog> gives focus-in, a Tab trap, Escape, an inert background,
	// focus back to the trigger, and the top layer (nothing's z-index beats it).
	import type { Snippet } from 'svelte';
	import { EyeIcon, XIcon } from 'phosphor-svelte';
	import { beforeNavigate } from '$app/navigation';
	import { page } from '$app/state';
	import { auth } from '$lib/auth.svelte';
	import { DRAWER_ID, navDrawer } from '$lib/navDrawer.svelte';
	import { globalDestinations, isCurrent } from '$lib/route';
	import { runsStore } from '$lib/runsStore.svelte';
	import { updates } from '$lib/updates.svelte';

	let { children }: { children?: Snippet } = $props();

	let dlg = $state<HTMLDialogElement>();
	const global = $derived(globalDestinations(runsStore.activeCount, updates.count));

	$effect(() => {
		if (!dlg) return;
		if (navDrawer.open && !dlg.open) dlg.showModal();
		else if (!navDrawer.open && dlg.open) dlg.close();
	});
	// Before, not after: the drawer goes as the tap lands, and is no longer open
	// when this component unmounts on the way into a run.
	beforeNavigate(() => navDrawer.dropped());
	$effect(() => {
		const onPop = () => navDrawer.dropped();
		addEventListener('popstate', onPop);
		return () => removeEventListener('popstate', onPop);
	});
	// Backdrop taps close. So does choosing the page already shown: navigating
	// would replace the drawer's entry with a copy of the one below it, and the
	// next Back would look like it did nothing.
	function onclick(e: MouseEvent) {
		const a = e.target instanceof Element ? e.target.closest('a') : null;
		if (a) {
			const to = new URL(a.href);
			if (to.pathname + to.search !== page.url.pathname + page.url.search) return;
			e.preventDefault();
		} else if (e.target !== dlg) return;
		navDrawer.hide();
	}
	// Unmounted while open (rotated past the phone breakpoint): pop the entry.
	$effect(() => () => navDrawer.hide());
</script>

<!-- Every close path (Escape, scrim, ✕, Back) ends in navDrawer.hide() or
     comes from it; the click target is the dialog itself only on ::backdrop. -->
<dialog
	bind:this={dlg}
	id={DRAWER_ID}
	class="drawer"
	aria-label="Menu"
	data-sveltekit-replacestate
	onclose={() => navDrawer.hide()}
	{onclick}
>
	<div class="head">
		<button class="close" aria-label="Close menu" onclick={() => navDrawer.hide()}>
			<XIcon size={20} weight="bold" />
		</button>
		<span class="brand">amplio</span>
	</div>
	<nav aria-label="Main">
		{#if children}
			{@render children()}
			<div class="sep" role="separator"></div>
		{/if}
		{#each global as d (d.href)}
			{@const current = isCurrent(d.href, page.url)}
			<a href={d.href} class="drawer-item" class:active={current} aria-current={current ? 'page' : undefined}>
				<d.icon size={20} weight={current ? 'fill' : 'regular'} />
				<span>{d.label}</span>
				{#if d.count}<span class="count {d.tone}">{d.count}</span>{/if}
			</a>
		{/each}
	</nav>
	{#if auth.ready && !auth.authed}
		<p class="readonly">
			<EyeIcon size={16} weight="bold" />
			Readonly view{auth.user ? ` of ${auth.user}'s runs` : ''}
		</p>
	{/if}
</dialog>

<style>
	/* Rendered on phones only; the media query keeps the rules with the tokens. */
	@media (max-width: 640px) {
		.drawer {
			margin: 0;
			inset: 0 auto 0 0;
			width: var(--drawer-w);
			max-width: none;
			height: 100dvh;
			max-height: none;
			padding: 0 0 var(--sp-3);
			display: flex;
			flex-direction: column;
			background: var(--bg-elev);
			color: var(--text);
			border: none;
			border-right: 1px solid var(--border);
			border-radius: 0 var(--sheet-radius) var(--sheet-radius) 0;
			overflow-y: auto;
			overscroll-behavior: contain;
			transform: translateX(-100%);
			transition:
				transform 200ms ease-out,
				overlay 200ms allow-discrete,
				display 200ms allow-discrete;
		}
		.drawer:not([open]) {
			display: none;
		}
		.drawer[open] {
			transform: none;
		}
		@starting-style {
			.drawer[open] {
				transform: translateX(-100%);
			}
		}
		.drawer::backdrop {
			background: var(--scrim);
			opacity: 0;
			transition:
				opacity 200ms,
				overlay 200ms allow-discrete,
				display 200ms allow-discrete;
		}
		.drawer[open]::backdrop {
			opacity: 1;
		}
		@starting-style {
			.drawer[open]::backdrop {
				opacity: 0;
			}
		}
		/* Lines up with the bar it covers: ✕ where ☰ was, wordmark where the title was. */
		.head {
			display: flex;
			align-items: center;
			gap: var(--sp-1);
			flex-shrink: 0;
			height: var(--bar-h);
			padding: 0 var(--sp-2) 0 var(--sp-1);
			border-bottom: 1px solid var(--border);
		}
		.close {
			display: inline-flex;
			align-items: center;
			justify-content: center;
			width: var(--tap);
			height: var(--tap);
			min-height: 0;
			padding: 0;
			background: none;
			border: none;
			color: var(--text);
		}
		.brand {
			font-size: var(--fs-lg);
			font-weight: 600;
			letter-spacing: 0.02em;
		}
		nav {
			display: flex;
			flex-direction: column;
			gap: 2px;
			padding: var(--sp-2) var(--sp-2) 0;
		}
		nav :global(.drawer-head) {
			margin: var(--sp-2) var(--sp-3) var(--sp-1);
			font-size: var(--fs-xs);
			color: var(--text-dim);
			text-transform: uppercase;
			letter-spacing: 0.04em;
		}
		nav :global(.drawer-item) {
			display: flex;
			align-items: center;
			gap: var(--sp-3);
			min-height: var(--row-h);
			padding: 0 var(--sp-3);
			border-radius: var(--radius-sm);
			color: var(--text);
			font-size: var(--fs-md);
		}
		nav :global(.drawer-item > span:first-of-type) {
			flex: 1;
			min-width: 0;
			overflow: hidden;
			text-overflow: ellipsis;
			white-space: nowrap;
		}
		nav :global(.drawer-item:hover) {
			text-decoration: none;
		}
		@media (hover: hover) {
			nav :global(.drawer-item:not(.active):hover) {
				background: var(--bg-elev2);
			}
		}
		nav :global(.drawer-item.active) {
			color: var(--accent);
			background: color-mix(in srgb, var(--accent) 12%, transparent);
		}
		/* Trailing meta: "wide" hints, counts. */
		nav :global(.drawer-item .trail),
		.count {
			font-size: var(--fs-xs);
			color: var(--text-dim);
		}
		.count {
			font-family: var(--mono);
		}
		.count.active {
			color: var(--ok);
		}
		.count.unread {
			color: var(--accent);
		}
		.sep {
			height: 1px;
			margin: var(--sp-2) var(--sp-3);
			background: var(--border);
		}
		.readonly {
			display: flex;
			align-items: center;
			gap: var(--sp-2);
			margin: auto 0 0;
			padding: var(--sp-4) calc(var(--sp-2) + var(--sp-3)) 0;
			font-size: var(--fs-sm);
			color: var(--text-dim);
		}
	}
</style>
