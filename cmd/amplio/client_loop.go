// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"amplio/internal/config"
	"amplio/internal/event"
	"amplio/internal/loop"
	"amplio/internal/util"
	"amplio/internal/verify"

	"github.com/spf13/cobra"
)

// clientLoopCmd groups the loop operations: a prompt re-sent to a session on a
// schedule. The server stores and fires them; this command only builds the
// schedule from flags, the way the web form does.
func clientLoopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "loop",
		Short: "Re-send a prompt to a session on a schedule (add, edit, list, stop)",
		Long: "A loop re-sends a prompt to a session every interval, as operator input:" +
			" each tick wakes the session, even a concluded one. Ticks are at least a" +
			" minute apart, and those missed while the server is down are not replayed." +
			" Cancelling the session (or its run) ends its loops.",
	}
	cmd.AddCommand(clientLoopAddCmd(), clientLoopEditCmd(), clientLoopListCmd(), clientLoopStopCmd())
	return cmd
}

// loopFlags are `loop add`'s schedule flags.
type loopFlags struct {
	every     string
	noSendNow bool
	ends      string
	times     int
	never     bool
	rrule     string
}

// schedule builds the loop's rule from the flags. With no end given, the
// server's [loop] expiry applies, fetched by defaultExpiry and written into the
// rule, so what is stored is what was asked for.
func (f loopFlags) schedule(cmd *cobra.Command, now time.Time, defaultExpiry func() (string, error)) (event.Schedule, error) {
	// A flag counts if given, except a bool given as false (--never=false).
	set := map[string]bool{
		"every": cmd.Flags().Changed("every"), "ends": cmd.Flags().Changed("ends"),
		"times": cmd.Flags().Changed("times"), "no-send-now": f.noSendNow, "never": f.never,
	}
	changed := func(names ...string) []string {
		var out []string
		for _, n := range names {
			if set[n] {
				out = append(out, "--"+n)
			}
		}
		return out
	}
	if cmd.Flags().Changed("rrule") {
		if others := changed("every", "no-send-now", "ends", "times", "never"); len(others) > 0 {
			return event.Schedule{}, fmt.Errorf("--rrule is the whole schedule; drop %s", strings.Join(others, ", "))
		}
		return rruleSchedule(f.rrule), nil
	}
	if f.every == "" {
		return event.Schedule{}, fmt.Errorf("--every is required (e.g. --every 30m), or give the whole schedule with --rrule")
	}
	every, err := util.ParseDuration(f.every)
	if err != nil {
		return event.Schedule{}, fmt.Errorf("--every: %w", err)
	}
	form := loop.Form{Every: every, SendNow: !f.noSendNow, Times: f.times}
	switch ends := changed("ends", "times", "never"); {
	case len(ends) > 1:
		return event.Schedule{}, fmt.Errorf("%s exclude each other: a loop ends after a duration, after a number of ticks, or never", strings.Join(ends, ", "))
	case set["times"] && f.times < 1:
		return event.Schedule{}, fmt.Errorf("--times must be at least 1")
	case set["ends"]:
		if form.Ends, err = util.ParseDuration(f.ends); err != nil {
			return event.Schedule{}, fmt.Errorf("--ends: %w", err)
		}
		if form.Ends <= 0 {
			return event.Schedule{}, fmt.Errorf("--ends must be positive; for a loop that doesn't end, use --never")
		}
	case len(ends) == 0:
		expiry, err := defaultExpiry()
		if err != nil {
			return event.Schedule{}, fmt.Errorf("read the server's default loop expiry (or pass --ends, --times or --never): %w", err)
		}
		if expiry != config.LoopExpiryNever {
			if form.Ends, err = util.ParseDuration(expiry); err != nil {
				return event.Schedule{}, fmt.Errorf("the server's default loop expiry %q: %w", expiry, err)
			}
		}
	}
	return form.Schedule(now)
}

// rruleSchedule is an --rrule schedule. A literal \n separates properties
// too, since a shell argument with real newlines is awkward to type.
func rruleSchedule(text string) event.Schedule {
	return event.Schedule{Kind: event.ScheduleRRule, Spec: loop.NormalizeSpec(strings.ReplaceAll(text, `\n`, "\n"))}
}

// serverLoopExpiry reads [loop] expiry from the server: a Go duration or "never".
func serverLoopExpiry(ctx context.Context) (string, error) {
	_, raw, err := clientDo(ctx, http.MethodGet, "/api/settings", nil)
	if err != nil {
		return "", err
	}
	var settings struct {
		Loop struct {
			Expiry string `json:"expiry"`
		} `json:"loop"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil || settings.Loop.Expiry == "" {
		return "", fmt.Errorf("no loop settings in the server's answer: %s", firstLine(string(raw)))
	}
	return settings.Loop.Expiry, nil
}

// loopVerifyUsage is --verify's usage on loop add and edit.
const loopVerifyUsage = "Verify each tick: each starts a new version of the session's verification, so an" +
	" independent verifier checks the session's conclusion to that tick (up to max-rounds verifier runs" +
	" per tick; [verify] defaults). Only for an agent that concludes"

// loopView is a loop as the API returns it.
type loopView struct {
	ID        string            `json:"id"`
	SessionID string            `json:"session_id"`
	Prompt    string            `json:"prompt"`
	Schedule  event.Schedule    `json:"schedule"`
	Verify    *event.LoopVerify `json:"verify,omitempty"`
	Summary   string            `json:"summary"`
	Ticks     int               `json:"ticks"`
	LastTick  *time.Time        `json:"last_tick"`
	NextTick  *time.Time        `json:"next_tick"`
}

func loopsPath(runID, session string) string {
	return "/api/runs/" + url.PathEscape(runID) + "/sessions/" + url.PathEscape(session) + "/loops"
}

// describe is the loop in a few words: its schedule, and the verification
// each tick starts.
func (l loopView) describe() string {
	if l.Verify == nil {
		return l.Summary
	}
	return l.Summary + ", verifying each tick (" + l.Verify.String() + ")"
}

// parseLoop reads a loop the server answered with.
func parseLoop(raw []byte) (loopView, error) {
	var l loopView
	if err := json.Unmarshal(raw, &l); err != nil {
		return l, fmt.Errorf("parse server response: %w", err)
	}
	return l, nil
}

func clientLoopAddCmd() *cobra.Command {
	var (
		f  loopFlags
		vf verifyFlags
	)
	cmd := &cobra.Command{
		Use:   "add <run-id> <session> PROMPT",
		Short: "Set a loop: send PROMPT to a session now, then every interval",
		Long: "Set a loop on a session. By default the first tick goes at once and the" +
			" loop ends after the server's [loop] expiry (7 days unless configured);" +
			" --no-send-now waits one interval first, and --ends, --times or --never" +
			" choose the end. With --times, the first tick counts. --rrule takes a whole" +
			" iCalendar schedule instead (DTSTART, then RRULE/RDATE/EXDATE lines," +
			" separated by newlines, spaces or a literal \\n); floating times in it are" +
			" the server's local time. --verify (or any --verify-* flag) verifies each" +
			" tick. Prints the loop id on stdout.",
		Example: "  amplio client loop add $RUN main-agent --every 30m \"check the eval and report changes\"\n" +
			"  amplio client loop add $RUN main-agent --every 2h --no-send-now --times 5 \"summarize new failures\"\n" +
			"  amplio client loop add $RUN main-agent --every 1d --verify-criteria \"go test ./... passes\" \"run the integration tests\"\n" +
			"  amplio client loop add $RUN main-agent --rrule 'DTSTART:20261116T090000 RRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;BYHOUR=9;BYMINUTE=0' \"weekly status\"",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, session, prompt := args[0], args[1], args[2]
			sched, err := f.schedule(cmd, time.Now(), func() (string, error) { return serverLoopExpiry(cmd.Context()) })
			if err != nil {
				return err
			}
			body := map[string]any{"prompt": prompt, "schedule": sched}
			if v := vf.request(cmd); v != nil {
				body["verify"] = v
			}
			_, raw, err := clientDo(cmd.Context(), http.MethodPost, loopsPath(runID, session), body)
			if err != nil {
				return err
			}
			l, err := parseLoop(raw)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "loop %s set on %s: %s; next tick %s\n", l.ID, session, l.describe(), formatTick(l.NextTick))
			fmt.Fprintln(cmd.OutOrStdout(), l.ID)
			return nil
		},
	}
	vf.register(cmd, loopVerifyUsage)
	cmd.Flags().StringVar(&f.every, "every", "", "Interval between ticks, at least 1m, in whole minutes: 30m, 2h, 1d")
	cmd.Flags().BoolVar(&f.noSendNow, "no-send-now", false, "Send the first tick after one interval instead of at once")
	cmd.Flags().StringVar(&f.ends, "ends", "", "End this long after now, e.g. 7d or 12h")
	cmd.Flags().IntVar(&f.times, "times", 0, "End after this many ticks")
	cmd.Flags().BoolVar(&f.never, "never", false, "Never end (until stopped, the session is cancelled, or the run is archived or deleted)")
	cmd.Flags().StringVar(&f.rrule, "rrule", "", "The whole schedule as iCalendar text, instead of the flags above")
	return cmd
}

func clientLoopEditCmd() *cobra.Command {
	var (
		prompt, rrule string
		vf            verifyFlags
		noVerify      bool
	)
	cmd := &cobra.Command{
		Use:   "edit <run-id> <session> <loop-id>",
		Short: "Change a loop's prompt, schedule or verification",
		Long: "Change a loop: --prompt replaces its prompt, --rrule its whole schedule (as on" +
			" add; the next tick is the rule's first occurrence from now, and the tick count" +
			" carries on). --verify makes it verify each tick, and each --verify-* flag changes" +
			" that part of its request: the parts not given keep the loop's current values (or" +
			" the server's [verify] defaults, for a loop that doesn't verify yet). --no-verify" +
			" makes it stop; its next tick turns off the version the loop started, if it is" +
			" still in force. A change applies from the next tick.",
		Example: "  amplio client loop edit $RUN main-agent 1 --prompt \"check the eval and the logs\"\n" +
			"  amplio client loop edit $RUN main-agent 1 --verify-max-rounds 2   # keeps its criteria and model\n" +
			"  amplio client loop edit $RUN main-agent 1 --no-verify",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, session, id := args[0], args[1], args[2]
			flags := cmd.Flags()
			body := map[string]any{}
			if flags.Changed("prompt") {
				body["prompt"] = prompt
			}
			if flags.Changed("rrule") {
				body["schedule"] = rruleSchedule(rrule)
			}
			switch v := vf.request(cmd); {
			case v != nil && noVerify:
				return fmt.Errorf("--no-verify stops verifying the loop's ticks; it takes no --verify flags")
			case v != nil:
				current, err := currentLoop(cmd.Context(), runID, session, id)
				if err != nil {
					return err
				}
				body["verify"] = mergeVerify(current.Verify, v, flags.Changed)
			case noVerify:
				body["verify"] = nil // null: stop verifying
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: give --prompt, --rrule, --verify (or a --verify-* flag) or --no-verify")
			}
			_, raw, err := clientDo(cmd.Context(), http.MethodPatch, loopsPath(runID, session)+"/"+url.PathEscape(id), body)
			if err != nil {
				return err
			}
			l, err := parseLoop(raw)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "loop %s on %s changed: %s; next tick %s\n", l.ID, session, l.describe(), formatTick(l.NextTick))
			return nil
		},
	}
	cmd.Flags().StringVar(&prompt, "prompt", "", "The new prompt")
	cmd.Flags().StringVar(&rrule, "rrule", "", "The new schedule as iCalendar text (as on add)")
	vf.register(cmd, loopVerifyUsage)
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "Stop verifying the loop's ticks")
	return cmd
}

// currentLoop reads one of a session's active loops from the server.
func currentLoop(ctx context.Context, runID, session, id string) (loopView, error) {
	_, raw, err := clientDo(ctx, http.MethodGet, loopsPath(runID, session), nil)
	if err != nil {
		return loopView{}, err
	}
	var loops []loopView
	if err := json.Unmarshal(raw, &loops); err != nil {
		return loopView{}, fmt.Errorf("parse loops of %s: %w", session, err)
	}
	for _, l := range loops {
		if l.ID == id {
			return l, nil
		}
	}
	return loopView{}, fmt.Errorf("no active loop %s on %s", id, session)
}

// mergeVerify is the request an edit's verify flags ask for: the loop's
// current request (nil if it doesn't verify), with the parts given as flags
// (changed reports which) replaced.
func mergeVerify(current *event.LoopVerify, flags *verify.Request, changed func(string) bool) verify.Request {
	r := verify.Request{}
	if current != nil {
		rounds := current.MaxRounds
		r = verify.Request{Criteria: current.Criteria, Model: current.Model, MaxRounds: &rounds}
	}
	if changed("verify-criteria") {
		r.Criteria = flags.Criteria
	}
	if changed("verify-model") {
		r.Model = flags.Model
	}
	if changed("verify-max-rounds") {
		r.MaxRounds = flags.MaxRounds
	}
	return r
}

// formatTick shows a time as the server sent it: in the server's zone, like
// the schedule summary.
func formatTick(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func clientLoopListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list <run-id> [session]",
		Short: "List a run's active loops (or one session's)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := args[0]
			sessions := args[1:]
			if len(sessions) == 0 {
				var err error
				if sessions, err = runSessionIDs(cmd.Context(), runID); err != nil {
					return err
				}
			}
			loops := []loopView{}
			for _, s := range sessions {
				_, raw, err := clientDo(cmd.Context(), http.MethodGet, loopsPath(runID, s), nil)
				if err != nil {
					return err
				}
				var page []loopView
				if err := json.Unmarshal(raw, &page); err != nil {
					return fmt.Errorf("parse loops of %s: %w", s, err)
				}
				loops = append(loops, page...)
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(loops)
			}
			return printLoops(cmd.OutOrStdout(), cmd.ErrOrStderr(), loops)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the loops as a JSON array")
	return cmd
}

// runSessionIDs lists a run's session ids.
func runSessionIDs(ctx context.Context, runID string) ([]string, error) {
	_, raw, err := clientDo(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(runID), nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse run detail: %w", err)
	}
	ids := make([]string, len(d.Sessions))
	for i, s := range d.Sessions {
		ids[i] = s.SessionID
	}
	return ids, nil
}

func printLoops(out, errOut io.Writer, loops []loopView) error {
	if len(loops) == 0 {
		fmt.Fprintln(errOut, "(no active loops)")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tLOOP\tSCHEDULE\tVERIFY\tTICKS\tNEXT\tPROMPT")
	for _, l := range loops {
		verifies := "-"
		if l.Verify != nil {
			verifies = "each tick"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			l.SessionID, l.ID, l.Summary, verifies, l.Ticks, formatTick(l.NextTick), firstLine(l.Prompt))
	}
	return w.Flush()
}

func clientLoopStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <run-id> <session> <loop-id>",
		Short: "Stop a loop",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, session, id := args[0], args[1], args[2]
			if _, _, err := clientDo(cmd.Context(), http.MethodDelete, loopsPath(runID, session)+"/"+url.PathEscape(id), nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "loop %s on %s stopped\n", id, session)
			return nil
		},
	}
}
