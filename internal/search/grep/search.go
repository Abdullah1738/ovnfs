package grep

import "context"

// SearchOptions configures [Search]: the matching and reporting subset of
// [Options].
type SearchOptions struct {
	Pattern    string
	Literal    bool
	IgnoreCase bool
	Multiline  bool
	// Mode is Content or Count; FilesWithMatches reports like Count.
	Mode          Mode
	Before, After int
	MaxColumns    int
	Offset        int
	MaxCount      int
}

// SearchResult is the outcome of [Search].
type SearchResult struct {
	// Matches are the selected matches (Content mode), Path empty.
	Matches []Match
	// MatchCount counts the matches found; the scan stops one match past
	// Offset+MaxCount.
	MatchCount int
	// LimitReached reports that MaxCount matches were returned or more
	// matches follow them.
	LimitReached bool
}

// Search searches one in-memory buffer with the matcher, scanner, and
// selection rules of [Grep], for callers that already hold the content.
// It errors only on an unsupported pattern.
func Search(content []byte, opts SearchOptions) (SearchResult, error) {
	mode := Count
	if opts.Mode == Content {
		mode = Content
	}
	q := Query{Pattern: opts.Pattern, Literal: opts.Literal, IgnoreCase: opts.IgnoreCase, Multiline: opts.Multiline, Limit: int64(len(content))}
	if mode == Content {
		q.Content, q.Before, q.After, q.MaxColumns = true, opts.Before, opts.After, opts.MaxColumns
		if opts.MaxCount > 0 {
			q.Cap, q.Probe = max(0, opts.Offset)+opts.MaxCount, true
		}
	}
	q = q.normal()
	c, err := q.compile()
	if err != nil {
		return SearchResult{}, err
	}
	s := &search{opts: &Options{Mode: mode, Offset: opts.Offset, MaxCount: opts.MaxCount}, q: q, c: c, params: q.params()}
	var t tally
	s.searchData(context.Background(), newScanner(c), &t, "", content, int64(len(content)))
	res := s.result(context.Background(), []*tally{&t}, true)
	if mode != Content {
		res.Matches = nil
	}
	return SearchResult{Matches: res.Matches, MatchCount: res.TotalMatches, LimitReached: res.LimitReached}, nil
}
