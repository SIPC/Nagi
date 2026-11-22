package logstore

import (
	"context"
	"encoding/json"
	"fmt"
)

// Page returns data, gaps and a watermark under the same gate. A filtered empty
// page may advance to that watermark, never beyond an unseen concurrent write.
type Page struct {
	State  State
	Events []Event
	Next   int64
	More   bool
}

func (s *Store) ReadPage(ctx context.Context, f Filter) (Page, error) {
	var p Page
	if err := s.lock(ctx); err != nil {
		return p, err
	}
	defer func() { <-s.gate }()
	var err error
	p.State, err = s.state(ctx)
	if err != nil {
		return p, err
	}
	if f.After > p.State.Last {
		return p, fmt.Errorf("日志游标超过当前来源末尾")
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	p.Next = p.State.Last
	if f.NewestFirst {
		p.Next = 0
	} else if f.Live && p.State.Last-f.After > liveScan {
		// Hundreds of thousands of imported records would otherwise be walked
		// in one call under the gate; the caller resumes at this bound.
		f.through = f.After + liveScan
		p.Next = f.through
	}
	events, err := s.query(ctx, f)
	if err != nil {
		return p, err
	}
	full := len(events) == f.Limit
	var size int
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			return p, err
		}
		if size+len(b) > 1<<20 {
			full = true
			break
		}
		size += len(b)
		p.Events = append(p.Events, e)
	}
	if full && len(p.Events) > 0 {
		p.Next = p.Events[len(p.Events)-1].Seq
	}
	p.More = full || !f.NewestFirst && p.Next < p.State.Last
	return p, nil
}
