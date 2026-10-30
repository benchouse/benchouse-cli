package langfuse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// manyTraces serves traces traces newest first, pageSize observations a
// page. Trace i (higher is newer) has a root and two children; trace i is in
// session "s<i/2>" for even sessions, and has no session otherwise, so
// sessions and bare traces mix. It counts the pages served. With rootOnly
// the session is on the root alone, as Langfuse v4 serves an OTel trace
// whose SDK did not propagate it (children read sessionId "").
func manyTraces(t *testing.T, traces int, served *int, rootOnly bool) *httptest.Server {
	var all []map[string]any
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := traces - 1; i >= 0; i-- {
		start := base.Add(time.Duration(i) * time.Minute)
		extra := map[string]any{}
		if (i/2)%2 == 0 {
			extra["sessionId"] = fmt.Sprintf("s%d", i/2)
		}
		id := fmt.Sprintf("t%d", i)
		ts := func(d time.Duration) string { return start.Add(d).Format(time.RFC3339) }
		child := extra
		if rootOnly {
			child = map[string]any{"sessionId": ""}
		}
		for _, o := range []map[string]any{
			obs(id+"c2", id, id+"r", "TOOL", "run_query", ts(20*time.Second), ts(21*time.Second), `{"sql":"select 1"}`, "", child),
			obs(id+"c1", id, id+"r", "GENERATION", "plan", ts(10*time.Second), ts(11*time.Second), "", "", child),
			obs(id+"r", id, "", "AGENT", "agent", ts(0), ts(30*time.Second), "q"+strconv.Itoa(i), "a", extra),
		} {
			all = append(all, o)
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*served++
		from, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		to := min(from+pageSize, len(all))
		meta := map[string]any{}
		if to < len(all) {
			meta["cursor"] = strconv.Itoa(to)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": all[from:to], "meta": meta})
	}))
}

func collectMany(t *testing.T, traces int, in agent.Inputs, opts agent.Options) (agent.Collected, int) {
	return collectManyShaped(t, traces, false, in, opts)
}

func collectManyShaped(t *testing.T, traces int, rootOnly bool, in agent.Inputs, opts agent.Options) (agent.Collected, int) {
	t.Helper()
	served := 0
	srv := manyTraces(t, traces, &served, rootOnly)
	defer srv.Close()
	full := agent.Inputs{"host": srv.URL, "public-key": "pk", "secret-key": "sk", "max-traces": "5000"}
	for k, v := range in {
		full[k] = v
	}
	a, _ := agent.Get("langfuse")
	res, err := agent.CollectAll(context.Background(), a.Collect, full, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res, served
}

func TestMaxConversationsKeepsTheMostRecentSessionsAndTraces(t *testing.T) {
	for _, rootOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("session on the root only %v", rootOnly), func(t *testing.T) {
			testMaxConversations(t, rootOnly)
		})
	}
}

func testMaxConversations(t *testing.T, rootOnly bool) {
	// 1,000 traces, 3,000 observations over 30 pages.
	res, served := collectManyShaped(t, 1000, rootOnly, nil, agent.Options{MaxConversations: 5})
	got := rows(t, res)
	// Newest first: t999, t998 (no session: one conversation each), t997 and
	// t996 (both s498: one), t995, t994 (no session): five conversations,
	// six traces.
	want := []string{"t999", "t998", "t997", "t996", "t995", "t994"}
	if len(got) != len(want) {
		t.Fatalf("turns = %d: %v", len(got), keys(got))
	}
	for _, id := range want {
		if got[id] == nil {
			t.Errorf("missing %s (got %v)", id, keys(got))
		} else if ex := got[id]["extra"].(map[string]any); ex["observations"] != float64(3) {
			t.Errorf("%s lost observations: %v", id, ex)
		}
	}
	if !res.Sampled || res.Conversations != 5 {
		t.Errorf("sample: %v %d", res.Sampled, res.Conversations)
	}
	// The caps are applied at a trace's root (which carries the session),
	// so the page after the cap completes the traces already started.
	if served > 3 {
		t.Errorf("served %d pages for 5 conversations", served)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "--max-conversations 5") {
		t.Errorf("notes = %v", res.Notes)
	}
}

// Without a conversation cap every trace is read, emitted as it completes:
// the hard --max-traces cap still bounds the read.
func TestNoCapReadsEverythingAndMaxTracesStillBounds(t *testing.T) {
	res, served := collectMany(t, 1000, nil, agent.Options{})
	if got := rows(t, res); len(got) != 1000 || served != 30 {
		t.Fatalf("turns = %d, pages = %d", len(got), served)
	}
	res, _ = collectMany(t, 1000, agent.Inputs{"max-traces": "250"}, agent.Options{})
	got := rows(t, res)
	if len(got) != 250 || got["t750"] == nil || got["t749"] != nil {
		t.Fatalf("turns = %d", len(got))
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
