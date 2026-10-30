package pgread

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func TestTimeReadsRowToJSONRenderings(t *testing.T) {
	want := time.Date(2026, 10, 2, 12, 0, 0, 123000000, time.UTC)
	for _, s := range []string{
		"2026-10-02T12:00:00.123+00:00", // timestamptz, session TimeZone UTC
		"2026-10-02T14:00:00.123+02:00", // timestamptz, another session TimeZone
		"2026-10-02T12:00:00.123+00",    // offset in hours only
		"2026-10-02T12:00:00.123",       // timestamp without time zone
	} {
		got, ok := Time(s)
		if !ok || !got.Equal(want) {
			t.Errorf("Time(%q) = %v, %v", s, got, ok)
		}
	}
	if _, ok := Time("yesterday"); ok {
		t.Error("parsed nonsense")
	}
}

func TestJSONTextReadsTextAndJSONB(t *testing.T) {
	if got := JSONText(json.RawMessage(`"[{\"a\":1}]"`)); string(got) != `[{"a":1}]` {
		t.Errorf("text column = %s", got)
	}
	if got := JSONText(json.RawMessage(`[{"a":1}]`)); string(got) != `[{"a":1}]` {
		t.Errorf("jsonb column = %s", got)
	}
	if JSONText(json.RawMessage(`null`)) != nil || JSONText(nil) != nil {
		t.Error("null is not empty")
	}
}

func TestSinceDefaultsToTheEpoch(t *testing.T) {
	if !Since(agent.Options{}).Equal(time.Unix(0, 0)) {
		t.Error("zero Since should read everything")
	}
}

func TestConnURLRequiresTLSForRemoteHosts(t *testing.T) {
	for _, c := range []struct {
		in   agent.Inputs
		want string
	}{
		{agent.Inputs{"host": "db.example.com", "sslmode": "auto"}, "sslmode=require"},
		{agent.Inputs{"host": "db.example.com"}, "sslmode=require"},
		{agent.Inputs{"host": "localhost", "sslmode": "auto"}, "sslmode=prefer"},
		{agent.Inputs{"host": "db.example.com", "sslmode": "auto", "sslrootcert": "/etc/ca.pem"}, "sslmode=verify-full"},
		{agent.Inputs{"host": "db.example.com", "sslmode": "verify-ca"}, "sslmode=verify-ca"},
	} {
		u, err := url.Parse(ConnURL(c.in, nil))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(u.RawQuery, c.want) || u.Query().Get("default_transaction_read_only") != "on" {
			t.Errorf("%v: query = %s", c.in, u.RawQuery)
		}
		if ca := c.in.Get("sslrootcert"); u.Query().Get("sslrootcert") != ca {
			t.Errorf("%v: sslrootcert = %q", c.in, u.Query().Get("sslrootcert"))
		}
	}
	u, _ := url.Parse(ConnURL(agent.Inputs{"host": "::1", "user": "u", "password": "p@ss/w?rd"}, url.Values{"timezone": {"UTC"}}))
	if u.Host != "[::1]:5432" || u.Query().Get("timezone") != "UTC" {
		t.Errorf("url = %s", u.Redacted())
	}
	if pw, _ := u.User.Password(); pw != "p@ss/w?rd" {
		t.Errorf("password = %q", pw)
	}
}

func TestFieldsDefaultToAuto(t *testing.T) {
	a := agent.Agent{Name: "x", Fields: Fields("X", nil)}
	in := agent.Inputs{"database": "d", "user": "u"}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	if in["sslmode"] != "auto" {
		t.Errorf("sslmode = %q", in["sslmode"])
	}
}
