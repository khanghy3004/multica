package handler

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAgentEnvEncryptRoundtrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{AgentEnvBox: box}

	in := map[string]string{"GITHUB_TOKEN": "ghp_secret", "MY_KEY": "value123"}
	bytes, err := h.marshalCustomEnv(in)
	if err != nil {
		t.Fatal(err)
	}

	// Must be envelope shape — NOT plaintext key-value
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(bytes, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["GITHUB_TOKEN"]; ok {
		t.Fatal("ciphertext leaked plaintext key name")
	}
	if _, ok := probe["_v"]; !ok {
		t.Fatal("missing envelope version marker")
	}
	if _, ok := probe["_ct"]; !ok {
		t.Fatal("missing ciphertext field")
	}

	out := h.unmarshalCustomEnv(db.Agent{CustomEnv: bytes})
	if out["GITHUB_TOKEN"] != "ghp_secret" || out["MY_KEY"] != "value123" {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
}

func TestAgentEnvLegacyPlaintextReadsBack(t *testing.T) {
	box, _ := secretbox.New(make([]byte, 32))
	h := &Handler{AgentEnvBox: box}
	// Pretend an old row written before encryption was wired
	legacy := []byte(`{"GITHUB_TOKEN":"ghp_legacy"}`)
	out := h.unmarshalCustomEnv(db.Agent{CustomEnv: legacy})
	if out["GITHUB_TOKEN"] != "ghp_legacy" {
		t.Fatalf("legacy plaintext read failed: %+v", out)
	}
}

func TestAgentEnvEmptyMapNoEnvelope(t *testing.T) {
	box, _ := secretbox.New(make([]byte, 32))
	h := &Handler{AgentEnvBox: box}
	bytes, err := h.marshalCustomEnv(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes) != "{}" {
		t.Fatalf("empty map should serialize as `{}`, got %s", bytes)
	}
}
