/*
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
*/

// A composer's options for the message being sent (ComposerOptions shows them,
// ComposerMenu or its ghost chips add them), the slash shortcuts that open
// their forms, and which of them can be added. Sending is lib/composerSend.

import { blockedOption, slashShortcut, type Shortcut, type VerifyFields } from "$lib/composerOptions";
import type { LoopFields } from "$lib/loopSchedule";
import type { MessageOptions } from "$lib/composerSend";

export type Option = Shortcut;

// The options a send took (see take), and which composer state they came from.
export interface Taken extends MessageOptions {
  readonly generation: number;
}

const FORM_OPEN = "Add or cancel the open form first.";
const DISCARD = "Discard the changes in the open form?";

export class ComposerState {
  verify = $state<VerifyFields | null>(null);
  loop = $state<LoopFields | null>(null);
  loopPrompt = $state(""); // the new-run form's loop prompt ("" = the task)
  // The new-run form's loop's own verification of each tick (the loop form's
  // "Verify each tick"); a message's Verify with Repeat is `verify` instead.
  loopVerify = $state<VerifyFields | null>(null);
  // Why something was refused (a shortcut, a send with a form open); "" if not.
  hint = $state("");
  // The open form has edits not yet applied (the form reports it).
  dirty = $state(false);
  // The form just opened should take the focus (not when a shortcut opened it).
  focusForm = $state(false);
  #open = $state<Option | null>(null);
  #generation = 0; // bumped by reset: options taken before belong elsewhere
  #offerVerify: () => boolean;
  #ownLoopVerify: boolean;

  // offerVerify: whether the target concludes (an interactive agent never does).
  // ownLoopVerify: the loop has its own verification (the new-run form, whose
  // Verify is the task's); else Verify with Repeat verifies each tick.
  constructor(offerVerify: () => boolean, { ownLoopVerify = false } = {}) {
    this.#offerVerify = offerVerify;
    this.#ownLoopVerify = ownLoopVerify;
  }

  get offersVerify(): boolean {
    return this.#offerVerify();
  }

  get ownLoopVerify(): boolean {
    return this.#ownLoopVerify;
  }

  // Verify is each tick's: the message repeats, and its verification goes
  // with the loop.
  get verifiesEachTick(): boolean {
    return !this.#ownLoopVerify && !!this.verify && !!this.loop;
  }

  get open(): Option | null {
    return this.#open;
  }

  // Why an option can't be added now, or "".
  blocked(which: Option): string {
    return blockedOption(which, this);
  }

  // show opens an option's form (null closes it). Unapplied edits in another
  // open form are dropped only if the operator agrees. Returns whether it did.
  show(which: Option | null, focus = true): boolean {
    if (which !== null && which === this.#open) {
      this.hint = ""; // already open: its edits stay, and so does `dirty`
      return true;
    }
    if (which && this.blocked(which)) {
      this.hint = this.blocked(which);
      return false;
    }
    if (this.#open && this.#open !== which && this.dirty && !confirm(DISCARD)) {
      return false;
    }
    this.#open = which;
    this.dirty = false;
    this.focusForm = focus && which !== null;
    this.hint = "";
    return true;
  }

  // close closes the open form at the operator's word (Cancel, Escape, or
  // after applying it): nothing to ask.
  close() {
    this.#open = null;
    this.dirty = this.focusForm = false;
    this.hint = "";
  }

  // dismiss is a click outside the open form: it closes, unless it has
  // unapplied edits; then, with `ask`, the operator decides, and without, it
  // stays open and the hint says why. Returns whether no form is open now.
  dismiss(ask = false): boolean {
    if (!this.#open) return true;
    if (this.dirty && !(ask && confirm(DISCARD))) {
      this.hint = FORM_OPEN;
      return false;
    }
    this.close();
    return true;
  }

  // slash handles a leading /verify or /loop: it opens that form and returns
  // the text without the word, returns false if the shortcut was refused (the
  // hint says why), or null if the text starts with none. While typing, it
  // doesn't open over another form's unapplied edits: the hint says so, and
  // sending (`final`) asks whether to discard them, once per send.
  slash(text: string, final = false): string | false | null {
    const s = slashShortcut(text, final);
    if (!s) {
      if (!final) this.hint = ""; // typing past a refused shortcut clears its hint
      return null;
    }
    if (!final && this.dirty && this.#open && this.#open !== s.cmd) {
      this.hint = FORM_OPEN;
      return false;
    }
    return this.show(s.cmd, false) ? s.rest : false;
  }

  // formOpen refuses a send while a form is open, so its edits aren't lost.
  formOpen(): boolean {
    if (this.#open) this.hint = FORM_OPEN;
    return this.#open !== null;
  }

  // take hands the options to a send and clears them; restore puts them back
  // after a failed send.
  take(): Taken {
    const o = { verify: this.verify, loop: this.loop, generation: this.#generation };
    this.verify = this.loop = null;
    this.hint = "";
    return o;
  }

  // restore puts back each option the failed send took, unless the composer
  // was reset since (it is another run's or session's now) or the operator
  // chose that option anew meanwhile.
  restore(o: Taken) {
    if (o.generation !== this.#generation) return;
    this.verify ??= o.verify;
    this.loop ??= o.loop;
  }

  reset() {
    this.#generation++;
    this.verify = this.loop = this.loopVerify = this.#open = null;
    this.loopPrompt = this.hint = "";
    this.dirty = this.focusForm = false;
  }
}
