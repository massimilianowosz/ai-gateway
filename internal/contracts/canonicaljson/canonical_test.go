package canonicaljson

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestCanonicalJSONV1Fixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Contract string `json:"contract"`
		Version  int    `json:"version"`
		Cases    []struct {
			Name      string `json:"name"`
			Value     any    `json:"value"`
			Canonical string `json:"canonical"`
			SHA256    string `json:"sha256"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Contract != "ubiquum.canonical_json" || fixture.Version != 1 {
		t.Fatalf("unsupported fixture contract %q v%d", fixture.Contract, fixture.Version)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			encoded, err := Marshal(item.Value)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != item.Canonical {
				t.Fatalf("canonical mismatch\nwant %s\n got %s", item.Canonical, encoded)
			}
			digest, err := SHA256(item.Value)
			if err != nil {
				t.Fatal(err)
			}
			if digest != item.SHA256 {
				t.Fatalf("digest mismatch: want %s, got %s", item.SHA256, digest)
			}
		})
	}
}

func TestRejectsNonFiniteAndUnsafeNumbers(t *testing.T) {
	for _, value := range []any{math.NaN(), math.Inf(1), math.Inf(-1), json.Number("9007199254740992")} {
		if _, err := Marshal(value); err == nil {
			t.Fatalf("expected %v to be rejected", value)
		}
	}
}
