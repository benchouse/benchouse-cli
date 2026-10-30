package pgread

import "testing"

func TestShortVersionAndLimitZero(t *testing.T) {
	for in, want := range map[string]string{
		"PostgreSQL 16.4 on x86_64-pc-linux-gnu, compiled by gcc": "PostgreSQL 16.4",
		"PostgreSQL 15.8, compiled by Visual C++":                 "PostgreSQL 15.8",
		"": "Postgres",
	} {
		if got := ShortVersion(in); got != want {
			t.Errorf("ShortVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if got := LimitZero("SELECT 1 -- note"); got != "SELECT * FROM (\nSELECT 1 -- note\n) AS probe LIMIT 0" {
		t.Errorf("LimitZero = %q", got)
	}
}
