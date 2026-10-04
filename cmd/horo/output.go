package main

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"text/tabwriter"
	"time"

	"horopter.dev/horopter/internal/client"
)

func writeChanges(
	w io.Writer, format string, loc *time.Location, rows iter.Seq2[client.Change, error],
) error {
	switch format {
	case "jsonl":
		return writeLines(w, rawChanges(rows))
	case "json":
		return writeArray(w, rawChanges(rows))
	default:
		return writeChangeTable(w, loc, rows)
	}
}

func writeChangeTable(w io.Writer, loc *time.Location, rows iter.Seq2[client.Change, error]) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "START\tDURATION\tSTATUS\tREF\tSUBJECT\tTYPE\tTITLE"); err != nil {
		return err
	}
	for ch, err := range rows {
		if err != nil {
			_ = tw.Flush()
			return err
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			ch.Start.In(loc).Format(time.RFC3339), duration(ch), ch.Status, ch.Ref, ch.Subject,
			ch.Type, ch.Title); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeShow(
	w io.Writer, format string, loc *time.Location, ch client.Change,
	events iter.Seq2[client.Event, error],
) error {
	switch format {
	case "jsonl":
		if _, err := fmt.Fprintf(w, "%s\n", ch.Raw); err != nil {
			return err
		}
		return writeLines(w, rawEvents(events))
	case "json":
		if _, err := fmt.Fprintf(w, `{"change":%s,"events":`, ch.Raw); err != nil {
			return err
		}
		if err := writeArray(w, rawEvents(events)); err != nil {
			return err
		}
		_, err := io.WriteString(w, "}\n")
		return err
	default:
		return writeShowTable(w, loc, ch, events)
	}
}

func writeShowTable(
	w io.Writer, loc *time.Location, ch client.Change, events iter.Seq2[client.Event, error],
) error {
	if _, err := fmt.Fprintf(w, "ref:      %s\nsubject:  %s\ntype:     %s\nstatus:   %s\n"+
		"title:    %s\nstart:    %s\nduration: %s\n\n", ch.Ref, ch.Subject, ch.Type, ch.Status,
		ch.Title, ch.Start.In(loc).Format(time.RFC3339), duration(ch)); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "OCCURRED\tTYPE\tACTOR\tTITLE\tID"); err != nil {
		return err
	}
	for ev, err := range events {
		if err != nil {
			_ = tw.Flush()
			return err
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			ev.OccurredAt.In(loc).Format(time.RFC3339), ev.Type, ev.Actor, ev.Title,
			ev.ID); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func duration(ch client.Change) string {
	if ch.End == nil {
		return "—"
	}
	return ch.End.Sub(ch.Start).Round(time.Second).String()
}

func rawChanges(rows iter.Seq2[client.Change, error]) iter.Seq2[json.RawMessage, error] {
	return func(yield func(json.RawMessage, error) bool) {
		for ch, err := range rows {
			if !yield(ch.Raw, err) {
				return
			}
		}
	}
}

func rawEvents(rows iter.Seq2[client.Event, error]) iter.Seq2[json.RawMessage, error] {
	return func(yield func(json.RawMessage, error) bool) {
		for ev, err := range rows {
			if !yield(ev.Raw, err) {
				return
			}
		}
	}
}

// writeLines writes each row as one line of JSON, egress's own object unchanged.
func writeLines(w io.Writer, rows iter.Seq2[json.RawMessage, error]) error {
	for raw, err := range rows {
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s\n", raw); err != nil {
			return err
		}
	}
	return nil
}

// writeArray writes rows as one JSON array, element by element, so a long result is not held
// in memory.
func writeArray(w io.Writer, rows iter.Seq2[json.RawMessage, error]) error {
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	sep := ""
	for raw, err := range rows {
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s%s", sep, raw); err != nil {
			return err
		}
		sep = ","
	}
	_, err := io.WriteString(w, "]\n")
	return err
}
