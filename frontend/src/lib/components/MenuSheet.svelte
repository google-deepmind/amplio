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
	import type { Snippet } from 'svelte';
	import { tick } from 'svelte';
	import { viewport } from '$lib/viewport.svelte';

	// A ⋮-style menu's surface. Phone: a modal <dialog> bottom sheet, like the
	// drawer: focus moves to the first item, Tab stays inside, the page behind
	// is inert, Escape and a scrim tap call onclose. Desktop: the caller's
	// popover markup, rendered as is.
	let {
		label,
		trigger,
		onclose,
		children
	}: { label: string; trigger?: HTMLElement; onclose: () => void; children: Snippet } = $props();

	const modal = viewport.isPhone;
	let dlg = $state<HTMLDialogElement>();
	$effect(() => {
		if (!dlg) return;
		dlg.showModal();
		dlg.querySelector<HTMLElement>('[role="menuitem"]:not(:disabled)')?.focus();
		// Closed by any path (item, scrim, Escape, Back): focus returns to the
		// trigger, unless something else (the rename field) has taken it.
		return () =>
			void tick().then(() => {
				if (document.activeElement === document.body) trigger?.focus();
			});
	});
	// A modal dialog lets Tab leave for the browser UI after the last item;
	// a menu sheet wraps instead.
	function wrapTab(e: KeyboardEvent) {
		if (e.key !== 'Tab' || !dlg) return;
		const f = [...dlg.querySelectorAll<HTMLElement>('button:not(:disabled)')];
		const edge = e.shiftKey ? f[0] : f[f.length - 1];
		if (document.activeElement !== edge) return;
		e.preventDefault();
		(e.shiftKey ? f[f.length - 1] : f[0])?.focus();
	}
	function backdropClick(e: MouseEvent) {
		if (e.target !== dlg) return;
		const r = dlg.getBoundingClientRect();
		const inside =
			e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom;
		if (!inside) onclose();
	}
</script>

{#if modal}
	<dialog
		bind:this={dlg}
		class="sheet"
		aria-label={label}
		oncancel={(e) => {
			e.preventDefault();
			onclose();
		}}
		onclick={backdropClick}
		onkeydown={wrapTab}
	>
		{@render children()}
	</dialog>
{:else}
	{@render children()}
{/if}

<style>
	/* ux/spec.md §2.2 "Menu → bottom sheet": full width, 12px top corners, a
	   32x4 handle, --scrim backdrop, at most 70dvh. Callers style the rows. */
	@media (max-width: 640px) {
		.sheet {
			position: fixed;
			inset: auto 0 0 0;
			margin: 0;
			width: auto;
			max-width: none;
			max-height: 70dvh;
			overflow-y: auto;
			overscroll-behavior: contain;
			padding: var(--sp-2);
			background: var(--bg-elev);
			color: var(--text);
			border: 1px solid var(--border);
			border-bottom: none;
			border-radius: var(--sheet-radius) var(--sheet-radius) 0 0;
		}
		.sheet::backdrop {
			background: var(--scrim);
		}
		.sheet::before {
			content: '';
			display: block;
			width: 32px;
			height: 4px;
			border-radius: 2px;
			background: var(--border);
			margin: 0 auto var(--sp-2);
		}
	}
</style>
