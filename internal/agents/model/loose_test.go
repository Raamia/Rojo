package model

import (
	"encoding/json"
	"testing"
)

// The exact shape that killed the first live GPT-5.2 job: numeric step ids.
func TestLooseString_AcceptsAModelsNumericIDs(t *testing.T) {
	var out struct {
		Steps []struct {
			ID LooseString `json:"id"`
		} `json:"steps"`
	}
	body := `{"steps":[{"id":1},{"id":2},{"id":"3"}]}`
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("a plan with numeric ids must parse: %v", err)
	}
	for i, want := range []string{"1", "2", "3"} {
		if got := out.Steps[i].ID.String(); got != want {
			t.Errorf("step %d id = %q, want %q", i, got, want)
		}
	}
}

func TestLooseString_Scalars(t *testing.T) {
	tests := map[string]string{
		`"plain"`: "plain",
		`""`:      "",
		`1`:       "1",
		`42`:      "42",
		// The literal is kept rather than round-tripped through float64, so a
		// large id keeps its digits instead of becoming 1e+18.
		`1000000000000000000`: "1000000000000000000",
		`1.5`:                 "1.5",
		`-3`:                  "-3",
		`true`:                "true",
		`null`:                "",
	}
	for input, want := range tests {
		var got LooseString
		if err := json.Unmarshal([]byte(input), &got); err != nil {
			t.Errorf("unmarshal %s: %v", input, err)
			continue
		}
		if got.String() != want {
			t.Errorf("%s => %q, want %q", input, got, want)
		}
	}
}

// Deliberately narrow: an object or array where a string belongs is a genuinely
// different answer, not a formatting slip, and must still fail.
func TestLooseString_RejectsStructuredValues(t *testing.T) {
	for _, input := range []string{`{"a":1}`, `[1,2]`, `[]`} {
		var got LooseString
		if err := json.Unmarshal([]byte(input), &got); err == nil {
			t.Errorf("%s was accepted as a string (=> %q)", input, got)
		}
	}
}

// Round-tripping a plan emits the normalised string form, so a number in
// becomes a quoted string out.
func TestLooseString_MarshalsAsAString(t *testing.T) {
	var s LooseString
	if err := json.Unmarshal([]byte(`7`), &s); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"7"` {
		t.Errorf("marshalled %s, want \"7\"", b)
	}
}
