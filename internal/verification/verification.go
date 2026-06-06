package verification

type Result struct {
	Check    string `json:"check"`
	Passed   bool   `json:"passed"`
	Output   string `json:"output,omitempty"`
	Duration int64  `json:"duration_ms"`
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
