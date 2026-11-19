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
	// Where a composer card opens an option's form. Desktop and tablet: a
	// non-modal popover in the top layer, above the card (`band`) at the chip
	// or + it opened from, kept in the viewport (lib/anchoredPlace). Phone: a
	// bottom sheet, modal like the menus' (MenuSheet), that Back closes. A press
	// outside it (on the sheet: its scrim) is ondismiss's to decide, as the form
	// may hold unapplied edits; a press in the card is the card's own.
	import { untrack, type Snippet } from 'svelte';
	import { viewport } from '$lib/viewport.svelte';
	import { closeOnBack } from '$lib/pickerSheet.svelte';
	import { isBackdropClick } from '$lib/dialog';
	import { placeAbove } from '$lib/anchoredPlace';

	let {
		anchor,
		band,
		labelledby,
		ondismiss,
		oncancel,
		children
	}: {
		anchor: () => HTMLElement | null | undefined; // read at each placement: a chip may come and go
		band: HTMLElement | undefined;
		labelledby: string;
		ondismiss: (modal: boolean) => void;
		oncancel: () => void; // Back or Escape on the sheet, Escape with the focus nowhere
		children: Snippet;
	} = $props();

	// Fixed when it opens: a rotation doesn't turn one into the other.
	const sheet = viewport.isPhone;
	let pop = $state<HTMLElement>();
	let dlg = $state<HTMLDialogElement>();

	closeOnBack(
		() => sheet,
		() => oncancel()
	);

	$effect(() => {
		if (!sheet || !dlg) return;
		dlg.showModal();
	});

	$effect(() => {
		const el = pop;
		if (sheet || !el) return;
		// The top layer: above every stacking context of the page around it.
		if (typeof el.showPopover === 'function' && !el.matches(':popover-open')) el.showPopover();
		const place = () => {
			const b = band?.getBoundingClientRect();
			if (!b) return;
			const scrolled = el.scrollTop; // measuring it whole drops it
			el.style.maxHeight = '';
			const at = placeAbove(
				anchor()?.getBoundingClientRect().left ?? b.left,
				b,
				{ width: el.offsetWidth, height: el.offsetHeight },
				{ width: innerWidth, height: innerHeight }
			);
			el.style.left = `${at.left}px`;
			el.style.top = `${at.top}px`;
			el.style.maxHeight = at.maxHeight === null ? '' : `${at.maxHeight}px`;
			el.classList.toggle('scrolls', at.maxHeight !== null);
			el.scrollTop = scrolled;
		};
		untrack(place); // its reads aren't this effect's: it re-runs on layout changes instead
		const ro = new ResizeObserver(place);
		ro.observe(el);
		if (band) ro.observe(band);
		const outside = (e: PointerEvent) => {
			const t = e.target as Node;
			if (!el.contains(t) && !band?.contains(t)) ondismiss(false);
		};
		// The page's scrolling moves the card; its own (when it scrolls) doesn't.
		const onScroll = (e: Event) => {
			if (!el.contains(e.target as Node)) place();
		};
		// Escape after a press on its label or padding (the focus is on <body>)
		// cancels, as it does in a field; the form handles the rest.
		const onKey = (e: KeyboardEvent) => {
			if (e.key !== 'Escape' || e.defaultPrevented || document.activeElement !== document.body) return;
			e.preventDefault();
			oncancel();
		};
		addEventListener('resize', place);
		addEventListener('scroll', onScroll, true);
		document.addEventListener('pointerdown', outside, true);
		document.addEventListener('keydown', onKey);
		return () => {
			ro.disconnect();
			removeEventListener('resize', place);
			removeEventListener('scroll', onScroll, true);
			document.removeEventListener('pointerdown', outside, true);
			document.removeEventListener('keydown', onKey);
		};
	});
</script>

{#if sheet}
	<dialog
		bind:this={dlg}
		class="sheet"
		aria-labelledby={labelledby}
		oncancel={(e) => {
			e.preventDefault();
			oncancel();
		}}
		onclick={(e) => {
			if (isBackdropClick(e, dlg)) ondismiss(true);
		}}
	>
		{@render children()}
	</dialog>
{:else}
	<div bind:this={pop} class="pop" popover="manual" role="dialog" aria-labelledby={labelledby}>
		{@render children()}
	</div>
{/if}

<style>
	/* Out of the flow of the toolbar it sits in, top layer or not; shown
	   before showPopover, so the form can take the focus as it mounts. */
	.pop {
		display: block;
		position: fixed;
		z-index: 40;
		inset: auto;
		margin: 0;
		width: min(30rem, calc(100vw - 16px));
		height: auto;
		padding: 0.7rem 0.85rem 0.8rem;
		overflow: visible;
		color: var(--text);
		background: var(--bg-elev);
		border: 1px solid color-mix(in srgb, var(--text-dim) 30%, var(--border));
		border-radius: var(--radius-lg);
		box-shadow: 0 10px 32px rgb(0 0 0 / 0.5);
	}
	/* Only when it fits nowhere: a scrolling box would clip the model menu. */
	.pop:global(.scrolls) {
		overflow-y: auto;
		overscroll-behavior: contain;
	}
	/* A bottom sheet as MenuSheet's; the bottom offset lifts it above an iOS
	   keyboard (see StartRunForm). */
	@media (max-width: 640px) {
		.sheet {
			position: fixed;
			inset: auto 0 calc(100dvh - var(--app-h, 100dvh)) 0;
			margin: 0;
			width: auto;
			max-width: none;
			max-height: calc(var(--app-h, 100dvh) - var(--bar-h));
			overflow-y: auto;
			overscroll-behavior: contain;
			padding: var(--sp-2) var(--gutter) var(--sp-4);
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
			margin: 0 auto var(--sp-3);
		}
	}
</style>
