package agent

import "testing"

func TestCapFor(t *testing.T) {
	slow := Agent{DefaultMaxConversations: 150}
	plain := Agent{}
	for _, c := range []struct {
		a      Agent
		chosen int
		set    bool
		want   int
	}{
		{slow, 2000, false, 150}, // the agent's own default replaces the global one
		{slow, 2000, true, 2000}, // an explicit choice always wins
		{slow, 0, true, 0},       // including "no cap"
		{plain, 2000, false, 2000},
	} {
		if got := CapFor(c.a, c.chosen, c.set); got != c.want {
			t.Errorf("CapFor(%d default, chosen %d, set %v) = %d, want %d", c.a.DefaultMaxConversations, c.chosen, c.set, got, c.want)
		}
	}
}
