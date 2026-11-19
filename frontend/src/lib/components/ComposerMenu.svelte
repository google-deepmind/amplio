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
	// A composer card's "+": opens the form for an option of the message being
	// sent (the card shows it). "Verify the result" is offered only when the
	// target concludes: an interactive agent is never verified. An option that
	// can't be added now says why instead.
	import { PlusIcon, RepeatIcon, SealCheckIcon } from 'phosphor-svelte';
	import { tick } from 'svelte';
	import { beforeNavigate } from '$app/navigation';
	import MenuSheet from './MenuSheet.svelte';
	import { menuKeys, popover } from '$lib/dismiss';
	import type { ComposerState, Option } from '$lib/composer.svelte';

	let {
		composer,
		button = $bindable()
	}: {
		composer: ComposerState;
		button?: HTMLButtonElement; // bound out: the form opened from here is anchored to it, and gives it the focus back
	} = $props();
	const name = $derived(composer.offersVerify ? 'Verify the result, or repeat on a schedule' : 'Repeat on a schedule');

	let menuOpen = $state(false);
	let wrap = $state<HTMLElement>();
	const pop = popover(() => close());
	beforeNavigate(() => {
		if (pop.release()) menuOpen = false;
	});
	$effect(() => () => void pop.closed());

	function toggle(e: MouseEvent) {
		e.stopPropagation();
		if (menuOpen) close();
		// The menu opens over an open form only once that form is gone: one
		// with unapplied edits stays, and the hint says why.
		else if (composer.dismiss()) {
			menuOpen = true;
			if (wrap) pop.opened(wrap);
			// As a menu button does, so the arrows work at once (MenuSheet does
			// it for the phone's sheet).
			void tick().then(() => wrap?.querySelector<HTMLElement>('[role="menuitem"]')?.focus());
		}
	}
	function close() {
		menuOpen = false;
		pop.closed();
	}
	// Escape closes the menu, and only the menu (not a sheet the composer is in).
	function onKey(e: KeyboardEvent) {
		if (e.key !== 'Escape' || !menuOpen) return;
		e.preventDefault();
		close();
		button?.focus();
	}
	async function pick(which: Option, e: MouseEvent) {
		e.stopPropagation();
		menuOpen = false;
		await pop.closed(); // the phone's Back entry is popped before the form shows
		const why = composer.blocked(which);
		if (why) composer.hint = why;
		else composer.show(which);
	}
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="cmenu" bind:this={wrap} onkeydown={onKey}>
	<button
		type="button"
		class="plus"
		bind:this={button}
		aria-haspopup="menu"
		aria-expanded={menuOpen}
		aria-label={name}
		title={name}
		onclick={toggle}
	>
		<PlusIcon size={14} weight="bold" />
	</button>
	{#if menuOpen}
		<MenuSheet label="Message options" trigger={button} onclose={close}>
			<div class="menu" role="menu" aria-label="Message options" tabindex="-1" onkeydown={menuKeys}>
				{#if composer.offersVerify}
					<button
						type="button"
						role="menuitem"
						class="item"
						aria-disabled={!!composer.blocked('verify')}
						title={composer.blocked('verify') || undefined}
						onclick={(e) => pick('verify', e)}
					>
						<SealCheckIcon size={16} />
						<span>Verify the result…</span>
					</button>
				{/if}
				<button
					type="button"
					role="menuitem"
					class="item"
					aria-disabled={!!composer.blocked('loop')}
					title={composer.blocked('loop') || undefined}
					onclick={(e) => pick('loop', e)}
				>
					<RepeatIcon size={16} />
					<span>Repeat on a schedule…</span>
				</button>
			</div>
		</MenuSheet>
	{/if}
</div>

<style>
	.cmenu {
		position: relative;
		flex-shrink: 0;
	}
	.plus {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 2rem;
		height: 2rem;
		padding: 0;
		border-radius: var(--radius-pill);
		background: none;
		color: var(--text-dim);
	}
	.plus:hover,
	.plus[aria-expanded='true'] {
		color: var(--text);
	}
	/* Desktop: a popover above the button (the card sits at the bottom). */
	.menu {
		position: absolute;
		bottom: calc(100% + 0.3rem);
		left: 0;
		z-index: 20;
		display: flex;
		flex-direction: column;
		min-width: 12rem;
		padding: 0.25rem;
		background: var(--bg-elev2);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		box-shadow: 0 6px 24px rgb(0 0 0 / 0.4);
	}
	.item {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		background: none;
		border: none;
		text-align: left;
		padding: 0.45rem 0.6rem;
		white-space: nowrap;
	}
	.item:hover,
	.item:focus-visible {
		background: var(--bg-elev);
	}
	.item[aria-disabled='true'] {
		opacity: 0.5;
	}
	@media (max-width: 640px) {
		.plus {
			width: var(--tap);
			height: var(--tap);
		}
		/* Phone: MenuSheet makes it a bottom sheet; the rows fill it. */
		.menu {
			position: static;
			background: none;
			border: none;
			box-shadow: none;
			padding: 0;
		}
		.item {
			min-height: 48px;
			gap: var(--sp-3);
			font-size: var(--fs-body);
		}
	}
</style>
