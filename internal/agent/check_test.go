package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckHelpers(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 12: "12", 999: "999", 1000: "1,000", 1204: "1,204", 1234567: "1,234,567", -4500: "-4,500"} {
		if got := Thousands(n); got != want {
			t.Errorf("Thousands(%d) = %q, want %q", n, got, want)
		}
	}
	since := Options{Since: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	if got := Counted(1204, "chat", "chats", since); got != "1,204 chats since 2026-08-01" {
		t.Errorf("Counted = %q", got)
	}
	if got := Counted(1, "chat", "chats", Options{}); got != "1 chat in total" {
		t.Errorf("Counted = %q", got)
	}
	for n, want := range map[int64]string{512: "512 B", 2048: "2.0 KiB", 3 << 20: "3.0 MiB"} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
	if got := Clip(errors.New(strings.Repeat("x", 500))); len(got) != 243 {
		t.Errorf("Clip length = %d", len(got))
	}
}
