package main

import (
	"encoding/json"
	"io"
	"os"

	"horopter.dev/horopter/internal/core"
)

// runReport is what --report writes, for a wrapper that must not parse stderr.
type runReport struct {
	StartPosted bool            `json:"start_posted"`
	EndPosted   bool            `json:"end_posted"`
	Ref         *string         `json:"ref"`
	ProducerRef string          `json:"producer_ref"`
	Outcome     *core.Lifecycle `json:"outcome"`
	ExitCode    int             `json:"exit_code"`
	Reason      *string         `json:"reason"`
}

func (r *runReport) started(ref string) {
	r.StartPosted = true
	r.Ref = &ref
}

// endPosted also sets Ref: after an interrupted start, the end is how it is learnt.
func (r *runReport) endPosted(ref string) {
	r.EndPosted = true
	r.Ref = &ref
}

func (r *runReport) ended(lc core.Lifecycle) { r.Outcome = &lc }

func (r *runReport) because(why error) {
	s := why.Error()
	r.Reason = &s
}

// write only warns on failure: recording never blocks the work.
func (r *runReport) write(path string, stderr io.Writer) {
	if path == "" {
		return
	}
	data, err := json.Marshal(r)
	if err == nil {
		err = os.WriteFile(path, append(data, '\n'), 0o600)
	}
	if err != nil {
		sayf(stderr, "horo run: could not write the report: %v\n", err)
	}
}
