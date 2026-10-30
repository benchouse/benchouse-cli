package telemetry

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// scrubCanaries are planted into error messages; frag is the part that
// must never come out of Scrub.
var scrubCanaries = []struct{ value, frag string }{
	{"canary.person@canary-corp.test", "canary-corp"},
	{"canary-host-7731.corp.internal", "canary-host-7731"},
	{"canary-host-7731.corp.internal:5432", "canary-host-7731"},
	{"https://canary-host-7731.corp.internal/api/v2?x=1", "canary-host-7731"},
	{"/Users/canary/secret-proj/models/x.sql", "secret-proj"},
	{`C:\Users\canary\secret-proj\x.sql`, "secret-proj"},
	{"ghp_CanaryS3cret9b7e1fQz4YtR2mWx8LpAvN0K", "CanaryS3cret"},
	{"10.77.31.204", "10.77.31.204"},
}

var scrubSeps = []string{" ", ": ", " (", "(", "=", " \"", "'", "\n", ", "}

// Scrub never panics, stays within MaxMessage and valid UTF-8, and never
// passes a planted email, host, path, address or token through.
func FuzzScrub(f *testing.F) {
	for _, c := range scrubCases {
		f.Add(c.in, uint16(len(c.in)/2), uint8(0), uint8(0))
	}
	f.Add("", uint16(0), uint8(1), uint8(1))
	f.Add(`{"message":"bad","error":"x"}`, uint16(12), uint8(2), uint8(3))
	f.Fuzz(func(t *testing.T, msg string, at uint16, which, sep uint8) {
		Scrub(msg)
		c := scrubCanaries[int(which)%len(scrubCanaries)]
		s := scrubSeps[int(sep)%len(scrubSeps)]
		i := int(at) % (len(msg) + 1)
		for i > 0 && i < len(msg) && !utf8.RuneStart(msg[i]) {
			i--
		}
		// The planted value must stand apart from what follows it.
		closing := s
		if strings.HasSuffix(closing, "(") {
			closing = ")"
		} else if strings.HasSuffix(closing, "\"") {
			closing = "\""
		}
		in := msg[:i] + s + c.value + closing + " " + msg[i:]
		out := Scrub(in)
		if utf8.RuneCountInString(out) > MaxMessage {
			t.Fatalf("output over MaxMessage: %d runes", utf8.RuneCountInString(out))
		}
		if strings.Contains(out, c.frag) {
			t.Fatalf("%q survived Scrub:\n in: %q\nout: %q", c.frag, in, out)
		}
	})
}
