package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// Helpers for writing a CheckResult's facts the same way in every agent.

// Thousands renders n with comma separators: 1204 as "1,204".
func Thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Window names the capture window for a fact: "since 2026-08-01", or "in
// total" when the whole history is read.
func Window(opts Options) string {
	if opts.Since.IsZero() {
		return "in total"
	}
	return "since " + opts.Since.UTC().Format("2006-01-02")
}

// Counted is a fact like "1,204 chats since 2026-08-01". one and many are
// the singular and plural nouns.
func Counted(n int64, one, many string, opts Options) string {
	noun := many
	if n == 1 {
		noun = one
	}
	return fmt.Sprintf("%s %s %s", Thousands(n), noun, Window(opts))
}

// Bytes renders a size for a fact: "512 B", "3.2 KiB", "14.0 MiB".
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Clip shortens a server's error text for a warning or an error.
func Clip(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}

// Conversations is Counted for a count of conversations, saying when the
// window holds more than --max-conversations: "1,000,000 chats since
// 2026-08-01: the 2,000 most recent will be sent".
func Conversations(n int64, one, many string, opts Options) string {
	s := Counted(n, one, many, opts)
	if opts.MaxConversations > 0 && n > int64(opts.MaxConversations) {
		s += fmt.Sprintf(": the %s most recent will be sent", Thousands(int64(opts.MaxConversations)))
	}
	return s
}
