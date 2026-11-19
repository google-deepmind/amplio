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
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"amplio/internal/verify"

	"github.com/spf13/cobra"
)

// clientVerifyCmd is `client verify`: set, replace or clear a session's
// verification (PUT / DELETE .../verify).
func clientVerifyCmd() *cobra.Command {
	var (
		req verify.Request
		off bool
	)
	cmd := &cobra.Command{
		Use:   "verify <run-id> <session>",
		Short: "Have a session's conclusion checked by an independent verifier (or --off)",
		Long: "Turn verification on for an autonomous session: its next conclusion goes to a verifier" +
			" sub-agent first, and back to the session until a verifier passes it or the rounds run out." +
			" Each run starts a new version of the request (v1, v2, ...): a conclusion still with the" +
			" verifier for the old one is withdrawn, and the same verifier checks the next one. Unset" +
			" flags take the server's [verify] defaults. Prints the resulting state on stdout.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			if off && (flags.Changed("criteria") || flags.Changed("model") || flags.Changed("max-rounds")) {
				return fmt.Errorf("--off turns verification off; it takes no --criteria, --model or --max-rounds")
			}
			if !flags.Changed("max-rounds") {
				req.MaxRounds = nil
			}
			path := "/api/runs/" + url.PathEscape(args[0]) + "/sessions/" + url.PathEscape(args[1]) + "/verify"
			method, body := http.MethodPut, any(req)
			if off {
				method, body = http.MethodDelete, nil
			}
			_, raw, err := clientDo(cmd.Context(), method, path, body)
			if err != nil {
				return err
			}
			var rep verify.Report
			if err := json.Unmarshal(raw, &rep); err != nil {
				return fmt.Errorf("parse server response: %w", err)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), describeVerify(args[1], rep))
			fmt.Fprintln(cmd.OutOrStdout(), rep.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&req.Criteria, "criteria", "", "What the verifier checks against (default: the session's current request)")
	cmd.Flags().StringVar(&req.Model, "model", "", "The verifier's model: a spec or #nickname (default: [verify] model, else the session's own)")
	req.MaxRounds = new(int)
	cmd.Flags().IntVar(req.MaxRounds, "max-rounds", 0, "Rejections before the session concludes unverified (default: the server's [verify] max_rounds; 0 = unbounded)")
	cmd.Flags().BoolVar(&off, "off", false, "Turn verification off (withdraws a conclusion still with the verifier)")
	return cmd
}

// describeVerify is the human line for a verification report.
func describeVerify(session string, r verify.Report) string {
	if r.State == verify.StatusOff {
		return "verification is off for " + session
	}
	var parts []string
	if r.Version > 0 { // 0 from a server before versions
		parts = append(parts, verify.Label{Version: r.Version, Candidate: r.Candidate}.String())
	}
	if r.Criteria != "" {
		parts = append(parts, "criteria: "+r.Criteria)
	}
	if r.Model != "" {
		parts = append(parts, "model: "+r.Model)
	}
	if r.MaxRounds > 0 {
		parts = append(parts, fmt.Sprintf("round %d of %d", r.Round, r.MaxRounds))
	} else {
		parts = append(parts, fmt.Sprintf("round %d, unbounded", r.Round))
	}
	if r.Verifier != "" {
		parts = append(parts, "verifier: "+r.Verifier)
	}
	return fmt.Sprintf("verification %s for %s (%s)", r.State, session, strings.Join(parts, "; "))
}
