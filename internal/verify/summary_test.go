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

package verify

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/event"
)

func TestSummarize(t *testing.T) {
	set := &event.VerifyEvent{Action: event.VerifySet, By: "boss", Criteria: "tests pass", MaxRounds: 3, Model: "#judge"}
	if s := Summarize(Derive(recs(final("hi")))); s != nil {
		t.Errorf("never verified: %+v, want nil", s)
	}
	for _, c := range []struct {
		name string
		evs  []event.Event
		want Summary
	}{
		{"set", []event.Event{set}, Summary{Status: StatusOn, Version: 1, Round: 1, MaxRounds: 3, Model: "#judge"}},
		{"verifying, round 2", []event.Event{set, final("v1"), ve("verifying", "v", 1), ve("rejected", "v", 1), final("v2"),
			ve("verifying", "v", 2)}, Summary{Status: StatusVerifying, Version: 1, Round: 2, MaxRounds: 3, Verifier: "v", Model: "#judge"}},
		{"passed", []event.Event{set, final("v1"), ve("verifying", "v", 1), ve("passed", "v", 1)},
			Summary{Status: StatusPassed, Version: 1, Round: 1, MaxRounds: 3, Verifier: "v", Model: "#judge"}},
		{"cleared", []event.Event{set, &event.VerifyEvent{Action: event.VerifyClear, By: "boss"}},
			Summary{Status: StatusOff, Version: 1, MaxRounds: 3, Model: "#judge"}},
		{"replaced, then passed", []event.Event{set, &event.VerifyEvent{Action: event.VerifyReplace, By: "operator", MaxRounds: 5},
			final("v1"), ve("verifying", "v", 1), ve("passed", "v", 1)},
			Summary{Status: StatusPassed, Version: 2, Round: 1, MaxRounds: 5, Verifier: "v"}},
	} {
		got := Summarize(Derive(recs(c.evs...)))
		if got == nil || *got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A Summary read back from metadata (JSON, so numbers come back as float64)
// is the one written.
func TestSummaryFrom_RoundTrip(t *testing.T) {
	want := Summary{Status: StatusVerifying, Version: 2, Round: 3, MaxRounds: 4, Verifier: "quick-rabbit", Model: "#judge"}
	b, _ := json.Marshal(map[string]any{SummaryMetadataKey: want})
	var meta map[string]any
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	if got := SummaryFrom(meta); got == nil || *got != want {
		t.Errorf("SummaryFrom = %+v, want %+v", got, want)
	}
	if got := SummaryFrom(map[string]any{"loops": []any{}}); got != nil {
		t.Errorf("SummaryFrom without the key = %+v, want nil", got)
	}
	// As written in memory, before a round trip through JSON.
	for _, v := range []any{want, &want, map[string]any{"status": StatusVerifying, "version": 2, "round": int64(3),
		"max_rounds": 4, "verifier": "quick-rabbit", "model": "#judge"}} {
		if got := SummaryFrom(map[string]any{SummaryMetadataKey: v}); got == nil || *got != want {
			t.Errorf("SummaryFrom(%T) = %+v, want %+v", v, got, want)
		}
	}
}

// Drain stops the writer: notes that keep coming while it drains don't race
// its wait (a WaitGroup Add from zero during Wait panics), Run returns, and
// what was noted before the stop is written.
func TestSummaryWriter_DrainStopsTheWriter(t *testing.T) {
	store, runID := newStore(t)
	addSession(t, store, runID, "worker", "boss", "standard_agent", db.SessionOngoing, nil)
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 2})
	for range 20 {
		w := NewSummaryWriter(store)
		ran := make(chan struct{})
		go func() { defer close(ran); w.Run(context.Background()) }()
		stop := make(chan struct{})
		noting := make(chan struct{})
		go func() {
			defer close(noting)
			for {
				select {
				case <-stop:
					return
				default:
					w.Observe(runID, "worker", &event.VerifyEvent{})
				}
			}
		}()
		time.Sleep(time.Millisecond)
		w.Drain(context.Background())
		close(stop)
		<-noting
		w.Observe(runID, "worker", &event.VerifyEvent{}) // wakes Run, which sees the stop
		select {
		case <-ran:
		case <-time.After(5 * time.Second):
			t.Fatal("Run didn't return after Drain")
		}
	}
	if got := summaryOf(t, store, runID, "worker"); got == nil || got.Status != StatusOn {
		t.Errorf("summary = %+v, want on", got)
	}
}

func summaryOf(t *testing.T, store db.Store, runID, sid string) *Summary {
	t.Helper()
	sess, err := store.GetSession(context.Background(), runID, sid)
	if err != nil {
		t.Fatal(err)
	}
	return SummaryFrom(sess.Metadata)
}

// The writer follows the session's VerifyEvents: a commit of one is noted, and
// Run rewrites the summary from the stream; other events don't wake it.
func TestSummaryWriter_FollowsTheStream(t *testing.T) {
	store, runID := newStore(t)
	addSession(t, store, runID, "worker", "boss", "standard_agent", db.SessionOngoing, nil)
	w := NewSummaryWriter(store)
	store.SetCommitListener(w.Observe)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitSummary := func(want *Summary) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			got := summaryOf(t, store, runID, "worker")
			if (got == nil && want == nil) || (got != nil && want != nil && *got == *want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("summary = %+v, want %+v", got, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	appendAll(t, store, runID, "worker", final("hi"))
	waitSummary(nil)
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 2})
	waitSummary(&Summary{Status: StatusOn, Version: 1, Round: 1, MaxRounds: 2})
	appendAll(t, store, runID, "worker", final("It is 42."), ve("verifying", "lynx", 1))
	waitSummary(&Summary{Status: StatusVerifying, Version: 1, Round: 1, MaxRounds: 2, Verifier: "lynx"})
	appendAll(t, store, runID, "worker", ve("rejected", "lynx", 1))
	waitSummary(&Summary{Status: StatusOn, Version: 1, Round: 2, MaxRounds: 2, Verifier: "lynx"})
}

// A verified session from before summaries were kept gets one at startup,
// found through its verifier; one that has a summary is left alone.
func TestSummaryWriter_Backfill(t *testing.T) {
	store, runID := newStore(t)
	addSession(t, store, runID, "worker", "boss", "standard_agent", db.SessionConcluded, nil)
	addSession(t, store, runID, "lynx", "worker", "verify_test_verifier", db.SessionConcluded,
		map[string]any{MetadataVerifierFor: "worker"})
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 3},
		final("It is 42."), ve("verifying", "lynx", 1), ve("passed", "lynx", 1))
	addSession(t, store, runID, "other", "boss", "standard_agent", db.SessionConcluded,
		map[string]any{SummaryMetadataKey: map[string]any{"status": "on", "version": 7}})
	addSession(t, store, runID, "owl", "other", "verify_test_verifier", db.SessionConcluded,
		map[string]any{MetadataVerifierFor: "other"})

	NewSummaryWriter(store).Backfill(context.Background())
	if got := summaryOf(t, store, runID, "worker"); got == nil || got.Status != StatusPassed || got.Verifier != "lynx" {
		t.Errorf("backfilled summary = %+v, want passed by lynx", got)
	}
	if got := summaryOf(t, store, runID, "other"); got == nil || got.Version != 7 {
		t.Errorf("an existing summary was rewritten: %+v", got)
	}
}

// The final record of a verification rides on the session's terminal write,
// which fires no commit listener for the session: its terminal status brings
// the summary up to date. A session never verified gets no summary written.
func TestSummaryWriter_TerminalStatus(t *testing.T) {
	store, runID := newStore(t)
	addSession(t, store, runID, "worker", "boss", "standard_agent", db.SessionOngoing, nil)
	addSession(t, store, runID, "plain", "boss", "standard_agent", db.SessionOngoing, nil)
	appendAll(t, store, runID, "worker", &event.VerifyEvent{Action: event.VerifySet, By: "boss", MaxRounds: 3},
		final("It is 42."), ve("verifying", "lynx", 1))
	w := NewSummaryWriter(store)
	store.SetCommitListener(w.Observe)
	store.SetSessionStatusChangedListener(w.OnSessionStatus)
	ctx := context.Background()
	for _, sid := range []string{"worker", "plain"} {
		var self []db.SelfEvent
		if sid == "worker" {
			self = []db.SelfEvent{{Event: ve("passed", "lynx", 1), Step: -1}}
		}
		if _, err := store.TerminateAndNotifyParent(ctx, runID, sid, "", db.SessionConcluded, "It is 42.", nil, self...); err != nil {
			t.Fatal(err)
		}
	}
	w.Drain(ctx)
	if got := summaryOf(t, store, runID, "worker"); got == nil || got.Status != StatusPassed {
		t.Errorf("after the terminal write: %+v, want passed", got)
	}
	sess, _ := store.GetSession(ctx, runID, "plain")
	if _, written := sess.Metadata[SummaryMetadataKey]; written {
		t.Errorf("a session never verified got a summary key: %v", sess.Metadata)
	}
}
