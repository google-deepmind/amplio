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
	// A composer card: the text box on top and, inside the same border, a
	// toolbar: on the left what this message carries (the + menu and the
	// option chips), on the right Send. An option's form opens from its chip
	// or the + as a popover (a sheet on a phone; OptionPopover). Under the
	// card, a line for why something was refused, or what the options cost.
	import { browser } from '$app/environment';
	import { tick, type Snippet } from 'svelte';
	import { PaperPlaneTiltIcon } from 'phosphor-svelte';
	import { viewport } from '$lib/viewport.svelte';
	import { serverSettings } from '$lib/serverSettings.svelte';
	import { optionsNote } from '$lib/composerOptions';
	import type { ComposerState, Option } from '$lib/composer.svelte';
	import type { MessageOptions } from '$lib/composerSend';
	import ComposerMenu from './ComposerMenu.svelte';
	import OptionChips from './OptionChips.svelte';
	import OptionForm from './OptionForm.svelte';
	import OptionPopover from './OptionPopover.svelte';
	import ComposerNotice from './ComposerNotice.svelte';
	import Spinner from './Spinner.svelte';

	let {
		onSend,
		placeholder = 'Send a message',
		draftKey,
		element = $bindable(),
		content = $bindable(''),
		composer,
		sendKey = 'enter',
		rows = 1,
		disabled = false,
		loopTitle,
		actions
	}: {
		// opts: the options taken from `composer` for this message (none without one).
		onSend: (content: string, opts: MessageOptions) => Promise<void>;
		placeholder?: string;
		draftKey?: string; // when set, persist the unsent draft in localStorage
		element?: HTMLTextAreaElement; // bound out so callers can focus the input
		content?: string; // the draft, bindable for a host that writes it (a generated prompt)
		// The message's options (verify, repeat): chips, a + menu and the slash
		// shortcuts. They go to onSend, and come back if it fails.
		composer?: ComposerState;
		// What sends from the keyboard: Enter (Shift+Enter is a newline), or, for
		// a longer prompt, Ctrl/Cmd+Enter (Enter is a newline).
		sendKey?: 'enter' | 'mod-enter';
		rows?: number; // the text box's height before it grows
		disabled?: boolean; // Send is off (the host is busy with the draft)
		loopTitle?: string; // the Repeat form's title
		actions?: Snippet; // more buttons, left of Send
	} = $props();

	// A soft keyboard has no Shift, so on phone Return is a newline and only the
	// button sends; the shortcut hint is only true where Enter sends.
	const enterSends = $derived(sendKey === 'enter' && !viewport.isPhone);
	const hint = $derived(enterSends ? `${placeholder} — Enter to send, Shift+Enter for newline` : placeholder);
	const mod = browser && /Mac|iP(hone|ad|od)/.test(navigator.userAgent) ? '⌘' : 'Ctrl';
	const sendTitle = $derived(sendKey === 'enter' ? 'Send (Enter)' : `Send (${mod}+Enter)`);
	const note = $derived(composer ? optionsNote(composer, serverSettings.verify.max_rounds) : '');
	const headId = $props.id();

	let busy = $state(false);
	let card = $state<HTMLFormElement>();
	let plus = $state<HTMLButtonElement>();

	// Restore a saved draft when the key (re)appears; survives reloads / outages.
	$effect(() => {
		if (browser && draftKey) content = localStorage.getItem(draftKey) ?? '';
	});
	// Grow with the draft, however it changed (typed, restored, generated),
	// once the box shows it; the CSS max-height caps it.
	$effect(() => {
		void content;
		void tick().then(() => {
			if (!element) return;
			element.style.height = 'auto';
			element.style.height = `${element.scrollHeight}px`;
		});
	});

	function saveDraft() {
		if (!browser || !draftKey) return;
		if (content) localStorage.setItem(draftKey, content);
		else localStorage.removeItem(draftKey);
	}

	function onInput() {
		const rest = composer?.slash(content);
		if (typeof rest === 'string') content = rest;
		saveDraft();
	}

	async function send() {
		// A shortcut opens its form rather than sending; a refused one (the hint
		// says why) doesn't send either.
		const rest = composer?.slash(content, true);
		if (typeof rest === 'string') {
			content = rest;
			saveDraft();
			return;
		}
		if (rest === false || composer?.formOpen()) return;
		const c = content.trim();
		if (!c || busy || disabled) return;
		const sent = content;
		const key = draftKey;
		// Clear immediately so focus stays and you can keep typing; restore the
		// draft (and the options) on failure for retry.
		content = '';
		saveDraft();
		const taken = composer?.take();
		busy = true;
		try {
			await onSend(sent, taken ?? { verify: null, loop: null });
		} catch {
			if (taken) composer?.restore(taken);
			if (key === draftKey) {
				content = sent;
				saveDraft();
			} else if (browser && key && !localStorage.getItem(key)) {
				localStorage.setItem(key, sent); // the box is another conversation's now: back to its own draft
			}
		} finally {
			busy = false;
			element?.focus();
		}
	}

	function onKey(e: KeyboardEvent) {
		// Escape here closes a form the slash shortcut opened, as a press outside it would.
		if (e.key === 'Escape' && composer?.open) {
			e.preventDefault();
			composer.dismiss();
			return;
		}
		if (e.key !== 'Enter' || e.isComposing) return; // IME-safe
		const sends = sendKey === 'enter' ? enterSends && !e.shiftKey : e.metaKey || e.ctrlKey;
		if (!sends) return;
		e.preventDefault();
		void send();
	}

	// After a form closes, the focus goes back where the operator was: its
	// chip, else the + it was opened from, else the text box (a shortcut).
	async function refocus(which: Option, fromMenu: boolean) {
		await tick();
		const chip = card?.querySelector<HTMLElement>(`[data-chip="${which}"]`);
		(chip ?? (fromMenu ? plus : element))?.focus();
	}
	function closed(which: Option) {
		const fromMenu = !!composer?.focusForm; // opened by a click, not a shortcut
		composer?.close();
		void refocus(which, fromMenu);
	}
	// A press on the card's empty space (its padding, the gap between the
	// buttons) goes to the text box, as if the whole card were the field; the
	// controls keep their own. Not while an option's form is open: a press
	// outside it is the form's to handle. Keyboard users reach the box by Tab.
	function pressCard(e: MouseEvent) {
		if (e.button !== 0 || composer?.open || !element) return;
		const t = e.target as Element;
		if (t === element || t.closest('button, a, input, select, textarea, label, [role="menu"], [role="dialog"], dialog, [data-chip]')) return;
		e.preventDefault(); // so the focus doesn't leave for the page first
		element.focus();
	}

	// A press outside the form. On a phone's sheet that is its scrim, so the
	// focus has nowhere else to be.
	function dismissed(modal: boolean) {
		const which = composer?.open;
		const fromMenu = !!composer?.focusForm;
		if (composer?.dismiss(modal) && modal && which) void refocus(which, fromMenu);
	}
</script>

<!-- The press handler is a mouse convenience over the text box, which keyboard
     users reach by Tab: no key handler or role is missing. -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<form
	class="mbox"
	bind:this={card}
	onmousedown={pressCard}
	onsubmit={(e) => {
		e.preventDefault();
		void send();
	}}
>
	<textarea
		class="input"
		bind:this={element}
		bind:value={content}
		placeholder={hint}
		enterkeyhint={enterSends ? 'send' : 'enter'}
		{rows}
		oninput={onInput}
		onkeydown={onKey}
	></textarea>
	<div class="tools">
		{#if composer}
			<ComposerMenu {composer} bind:button={plus} />
			<OptionChips {composer} dialog onremove={() => element?.focus()} />
			<!-- Here, so Tab goes from the chip into its form and on to Send. -->
			{#if composer.open}
				{@const which = composer.open}
				<OptionPopover
					anchor={() => card?.querySelector<HTMLElement>(`[data-chip="${which}"]`) ?? plus}
					band={card}
					labelledby={headId}
					ondismiss={dismissed}
					oncancel={() => closed(which)}
				>
					<!-- Why something was refused while the form is open, in the form's popover. -->
					{#if composer.hint}<p class="form-hint small" role="status" aria-live="polite">{composer.hint}</p>{/if}
					<OptionForm {composer} {loopTitle} {headId} ondone={closed} />
				</OptionPopover>
			{/if}
		{/if}
		<span class="grow"></span>
		{@render actions?.()}
		<button class="send" aria-label="Send" title={sendTitle} disabled={busy || disabled || !content.trim()}>
			{#if busy}<Spinner size={14} />{:else}<PaperPlaneTiltIcon size={16} weight="fill" />{/if}
		</button>
	</div>
</form>
<!-- Why something was refused, where the forms pop up (above the card); with a
     form open, it shows in the form's popover instead. -->
{#if composer?.hint && !composer.open}
	<ComposerNotice band={card} text={composer.hint} onclose={() => composer && (composer.hint = '')} />
{/if}
{#if note}
	<p class="line cost small">{note}</p>
{/if}

<style>
	/* The card's border is its focus ring while the text box has the focus, as
	   on the new-run form; its controls show their own. */
	.mbox {
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		padding: 0.45rem 0.5rem;
		background: var(--bg-elev);
		border: 1px solid color-mix(in srgb, var(--text-dim) 30%, var(--border));
		border-radius: var(--radius-lg);
		transition: border-color 0.12s ease;
		cursor: text; /* a press on its empty space focuses the text box (pressCard); buttons keep theirs */
	}
	.mbox:has(.input:focus) {
		border-color: var(--accent-dim);
	}
	.input {
		background: transparent;
		border: none;
		padding: 0.3rem 0.4rem;
		resize: none;
		max-height: 200px;
		overflow-y: auto;
		line-height: 1.4;
	}
	.input:focus {
		outline: none;
	}
	.tools {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		min-width: 0;
	}
	.grow {
		flex: 1;
	}
	.send {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex-shrink: 0;
		width: 2rem;
		height: 2rem;
		padding: 0;
		border-radius: var(--radius-pill);
		background: var(--accent);
		border-color: var(--accent);
		color: var(--on-accent);
	}
	.line {
		margin: 0;
	}
	.form-hint {
		margin: 0 0 0.5rem;
		color: var(--warn);
	}
	.cost {
		color: var(--warn);
	}

	@media (max-width: 640px) {
		.mbox {
			padding: var(--sp-1);
			gap: 0;
		}
		/* Keep the transcript in view while a long draft grows (keyboard open). */
		.input {
			max-height: min(200px, 30dvh);
			padding: var(--sp-2) var(--sp-2) var(--sp-1);
		}
		.tools {
			gap: var(--sp-1);
		}
		.send {
			width: var(--tap);
			height: var(--tap);
		}
	}
</style>
