package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func secretAgent(id string) Agent {
	return Agent{ID: id, Name: id, Fields: withSecretFiles([]Field{
		{Key: "host", Label: "Host"},
		{Key: "token", Label: "Access token", Secret: true},
		{Key: "space", Label: "Space", Optional: true, Lookup: func(_ context.Context, in Inputs) ([][2]string, error) {
			return [][2]string{{in.Get("token"), ""}}, nil
		}},
	})}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSecretFieldsGetAFileSibling(t *testing.T) {
	a := secretAgent("x")
	var keys []string
	for _, f := range a.Fields {
		keys = append(keys, f.Key)
	}
	if strings.Join(keys, ",") != "host,token,token-file,space" {
		t.Fatalf("fields = %v", keys)
	}
	file := a.Fields[2]
	if file.Secret || !file.Optional || file.Applies(Inputs{}) || file.EnvName() != "BENCHOUSE_TOKEN_FILE" {
		t.Errorf("file field = %+v", file)
	}
	// The secret is not prompted for when its file was given.
	if a.Fields[1].Applies(Inputs{"token-file": "/x"}) || !a.Fields[1].Applies(Inputs{}) {
		t.Error("token applies wrongly")
	}
}

func TestValidateReadsSecretFiles(t *testing.T) {
	a := secretAgent("x")
	in := Inputs{"host": "h", "token-file": writeFile(t, "s3cret\r\n")}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	if in["token"] != "s3cret" {
		t.Errorf("token = %q", in["token"])
	}
	// A token given directly wins.
	in = Inputs{"host": "h", "token": "direct", "token-file": writeFile(t, "file")}
	if err := a.Validate(in); err != nil || in["token"] != "direct" {
		t.Errorf("token = %q, %v", in["token"], err)
	}
	// Lookups see the token read from its file, before Validate runs.
	in = Inputs{"host": "h", "token-file": writeFile(t, "fromfile\n")}
	got, err := a.Fields[3].Lookup(context.Background(), in)
	if err != nil || got[0][0] != "fromfile" {
		t.Errorf("lookup saw %v, %v", got, err)
	}
}

func TestSecretFileErrors(t *testing.T) {
	a := secretAgent("x")
	for name, path := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "nope"),
		"empty":   writeFile(t, "\n"),
		"large":   writeFile(t, strings.Repeat("a", maxSecretFile+1)),
	} {
		err := a.Validate(Inputs{"host": "h", "token-file": path})
		if err == nil || !strings.Contains(err.Error(), "--token-file") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := a.Validate(Inputs{"host": "h"}); err == nil || !strings.Contains(err.Error(), "--token") {
		t.Errorf("missing token: err = %v", err)
	}
}

func TestExistingFileFieldIsNotShadowed(t *testing.T) {
	fs := withSecretFiles([]Field{{Key: "token", Secret: true}, {Key: "token-file", Label: "mine"}})
	if len(fs) != 2 || fs[1].Label != "mine" {
		t.Errorf("fields = %+v", fs)
	}
}
