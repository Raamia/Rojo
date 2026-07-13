package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// LooseString is a string that also accepts a JSON number or boolean.
//
// Models are inconsistent about whether an identifier is "1" or 1, and for a
// label the difference means nothing. A plain string field turns that into
// `cannot unmarshal number into Go struct field ... of type string`, which
// fails the whole job and throws away a paid model call over punctuation.
//
// This was not hypothetical: the first live GPT-5.2 run returned a plan whose
// step ids were numbers, and the job died in the planning step.
//
// Deliberately narrow. It coerces the scalar types a model plausibly confuses
// with a string, and nothing else: an object or an array where a string belongs
// is a genuinely different answer, not a formatting slip, and should still fail.
type LooseString string

func (s *LooseString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)

	// null leaves the zero value; the caller's own validation decides whether
	// an empty value is acceptable, which keeps that judgement in one place.
	if bytes.Equal(b, []byte("null")) {
		*s = ""
		return nil
	}

	// The ordinary case.
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*s = LooseString(str)
		return nil
	}

	// A number: keep the literal rather than round-tripping through float64,
	// so 1 stays "1" instead of becoming "1e+00" and a big id keeps its digits.
	if num, err := strconv.ParseFloat(string(b), 64); err == nil {
		_ = num
		*s = LooseString(b)
		return nil
	}

	if bytes.Equal(b, []byte("true")) || bytes.Equal(b, []byte("false")) {
		*s = LooseString(b)
		return nil
	}

	return fmt.Errorf("cannot read %s as a string", truncateJSON(b))
}

// String lets the value be used wherever a string is expected without a cast
// at every call site.
func (s LooseString) String() string { return string(s) }

// MarshalJSON writes a plain string, so anything that round-trips a plan
// through JSON emits the normalised form rather than the model's original
// spelling.
func (s LooseString) MarshalJSON() ([]byte, error) { return json.Marshal(string(s)) }

// truncateJSON keeps an error message readable when the offending value is a
// large object.
func truncateJSON(b []byte) string {
	const max = 60
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}
