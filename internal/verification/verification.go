package verification

type Result struct {
	Check    string `json:"check"`
	Passed   bool   `json:"passed"`
	Output   string `json:"output,omitempty"`
	Duration int64  `json:"duration_ms"`
	// Note qualifies a pass that deserves a caveat: tests that ran but found
	// nothing to run, a toolchain that was recognised but is not installed. It
	// exists because "passed" alone can be true while meaning much less than a
	// reader assumes, and the difference belongs in the record, not in a log
	// line that scrolls away.
	Note string `json:"note,omitempty"`
}

type Report struct {
	Results []Result `json:"results"`
}

func (r Report) AllPassed() bool {
	for _, res := range r.Results {
		if !res.Passed {
			return false
		}
	}
	return true
}
