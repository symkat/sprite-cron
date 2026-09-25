package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/symkat/sprite-cron/internal/app"
)

func TestKeygenBootstrapFormats(t *testing.T) {
	t.Setenv("SPRITE_CRON_KEYS", "invalid-existing-config")
	t.Setenv("SPRITE_CRON_DB", "/does/not/exist/db")
	seen := map[string]bool{}
	for _, args := range [][]string{nil, {"--fly-secret"}, {"--fly-secret"}} {
		var out bytes.Buffer
		if err := keygen(args, &out); err != nil {
			t.Fatal(err)
		}
		line := out.String()
		if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
			t.Fatal("output must be exactly one line")
		}
		value := strings.TrimSuffix(line, "\n")
		if len(args) > 0 {
			if !strings.HasPrefix(value, "SPRITE_CRON_KEYS=") {
				t.Fatal("missing import variable")
			}
			spec := strings.TrimPrefix(value, "SPRITE_CRON_KEYS=")
			vault, err := app.NewVault(spec, "v1")
			if err != nil {
				t.Fatal("imported keyring is unusable", err)
			}
			cipher, err := vault.Seal("probe", "roundtrip")
			if err != nil {
				t.Fatal(err)
			}
			plain, err := vault.Open("probe", cipher)
			if err != nil || plain != "roundtrip" {
				t.Fatal("key roundtrip failed")
			}
			var keys map[string]string
			if err = json.Unmarshal([]byte(spec), &keys); err != nil || len(keys) != 1 {
				t.Fatal("unexpected keyring")
			}
			value = keys["v1"]
		}
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != 32 {
			t.Fatal("expected base64 32-byte key")
		}
		if seen[value] {
			t.Fatal("key was reused")
		}
		seen[value] = true
	}
	for _, args := range [][]string{{"--unknown"}, {"unexpected"}} {
		var out bytes.Buffer
		if err := keygen(args, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid command emitted secret")
		}
	}
}
