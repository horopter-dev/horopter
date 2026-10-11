package main

import (
	"encoding/json"
	"io"
	"os"
)

// runReport is what --report writes: what horo run posted and how the change ended, for a
// wrapper such as a CI step summary that must not parse stderr. Outcome is null when the
// command was not run and no change was closed; reason is null only when the change was recorded
// in full.
type runReport struct {
	StartPosted bool    `json:"start_posted"`
	EndPosted   bool    `json:"end_posted"`
	Ref         *string `json:"ref"`
	ProducerRef string  `json:"producer_ref"`
	Outcome     *string `json:"outcome"`
	ExitCode    int     `json:"exit_code"`
	Reason      *string `json:"reason"`
}

func (r *runReport) started(ref string) {
	r.StartPosted = true
	r.Ref = &ref
}

// endPosted records a confirmed end and the stored ref it answered with: after an interrupted
// start, the end is how that ref is learnt.
func (r *runReport) endPosted(ref string) {
	r.EndPosted = true
	r.Ref = &ref
}

func (r *runReport) ended(word string) { r.Outcome = &word }

func (r *runReport) because(why error) {
	s := why.Error()
	r.Reason = &s
}

// write saves the report at path, if one was asked for. A report that cannot be written is
// warned about and does not change horo's exit code: recording never blocks the work.
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
