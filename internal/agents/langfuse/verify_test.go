package langfuse

import (
	"context"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func TestVerifyKeys(t *testing.T) {
	srv := checkServer(t, `{"data":[]}`, 0)
	defer srv.Close()
	in := agent.Inputs{"host": srv.URL, "public-key": "pk-lf-1", "secret-key": "sk-lf-1"}
	if fact, err := verifyKeys(context.Background(), in); err != nil || fact != "Connected to Langfuse at "+srv.URL+", project analytics-prod" {
		t.Errorf("good keys: %q, %v", fact, err)
	}
	in["secret-key"] = "sk-lf-WRONG"
	if _, err := verifyKeys(context.Background(), in); err == nil || !strings.Contains(err.Error(), "rejected the API keys") || strings.Contains(err.Error(), "WRONG") {
		t.Errorf("bad keys: %v", err)
	}
}

func TestKeysAreNotSwapped(t *testing.T) {
	a, _ := agent.Get("langfuse")
	err := a.Validate(agent.Inputs{"public-key": "sk-lf-123", "secret-key": "pk-lf-1"})
	if err == nil || !strings.Contains(err.Error(), "--public-key: that looks like the secret key") || strings.Contains(err.Error(), "123") {
		t.Errorf("swapped keys: %v", err)
	}
	if err := a.Validate(agent.Inputs{"public-key": "pk-lf-1", "secret-key": "sk-lf-1", "max-traces": "0"}); err == nil || !strings.Contains(err.Error(), "--max-traces") {
		t.Errorf("max-traces 0: %v", err)
	}
}
