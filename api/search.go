package api

// SearchArgs selects a search root and the traversal shared by grep and glob.
// Dir and Checkout are mutually exclusive; the CLI defaults Dir to its current
// directory. Searches honor ignore files by default, always prune .git, and
// prune node_modules unless requested or named by the search glob.
type SearchArgs struct {
	Dir         string `json:"dir,omitempty"`          // absolute ordinary folder or mounted checkout path
	Checkout    string `json:"checkout,omitempty"`     // named checkout, searched directly through its overlay
	Path        string `json:"path,omitempty"`         // optional slash-separated subpath relative to the root
	Hidden      bool   `json:"hidden,omitempty"`       // include dot-prefixed entries
	NoIgnore    bool   `json:"no_ignore,omitempty"`    // do not honor .gitignore, .ignore, or exclude files
	NodeModules bool   `json:"node_modules,omitempty"` // descend into node_modules even when not named by the glob
	Workers     int    `json:"workers,omitempty"`      // walking goroutines; zero selects the platform default
	MaxDepth    int    `json:"max_depth,omitempty"`    // root children have depth 1; zero means unlimited
}

// GrepArgs searches file contents below the selected root, or Path itself when
// it is a file. Patterns use the search engine's Rust-compatible regex dialect.
type GrepArgs struct {
	SearchArgs
	Pattern         string   `json:"pattern"`                      // regular expression, or plain text with Literal
	Literal         bool     `json:"literal,omitempty"`            // search Pattern as plain text
	IgnoreCase      bool     `json:"ignore_case,omitempty"`        // Unicode simple case folding
	Multiline       bool     `json:"multiline,omitempty"`          // allow matches across lines; report adjacent matching lines as blocks
	Glob            string   `json:"glob,omitempty"`               // scope root-relative paths: matching files plus files under matching directories; slash-free globs match at any depth
	Anchored        bool     `json:"anchored,omitempty"`           // match slash-free globs only at the root
	Exclude         []string `json:"exclude,omitempty"`            // skip matching files and directories; slash-free globs match at any depth, a leading / anchors to the root
	Type            string   `json:"type,omitempty"`               // file type name (rust, ts, py, ...) or extension/file name
	Mode            string   `json:"mode,omitempty"`               // empty/content, filesWithMatches, or count
	Before          int      `json:"before,omitempty"`             // context lines before each content match
	After           int      `json:"after,omitempty"`              // context lines after each content match
	MaxColumns      int      `json:"max_columns,omitempty"`        // fit reported lines into this many bytes, windowing matching lines around their first match; zero means unlimited
	Offset          int      `json:"offset,omitempty"`             // skip this many results in path order
	MaxCount        int      `json:"max_count,omitempty"`          // maximum returned results after Offset; zero means unlimited
	MaxCountPerFile int      `json:"max_count_per_file,omitempty"` // maximum content matches per file; zero means unlimited
	MaxFileSize     int64    `json:"max_file_size,omitempty"`      // leading bytes searched per file; zero defaults to 4 MiB
}

// ContextLine is a line surrounding a grep content match.
type ContextLine struct {
	Line int    `json:"line"` // 1-based line number
	Text string `json:"text"` // text without trailing whitespace, with invalid UTF-8 replaced
}

// GrepMatch is a content match, a matching file, or a per-file count.
type GrepMatch struct {
	Path      string        `json:"path"`             // root-relative; a file root uses its absolute native or checkout-virtual path
	Line      int           `json:"line"`             // 1-based first matching line; zero outside content mode
	Text      string        `json:"text"`             // matching line or multiline block; empty outside content mode
	Before    []ContextLine `json:"before,omitempty"` // preceding context in content mode
	After     []ContextLine `json:"after,omitempty"`  // following context in content mode
	Truncated bool          `json:"truncated"`        // MaxColumns cut Text
	Column    int           `json:"column,omitempty"` // 1-based character column of the first match when MaxColumns cut Text
	Count     int           `json:"count"`            // file match count in count mode; otherwise zero
}

// GrepReply contains results ordered by path, then line. Counts describe the
// searched portion only when a result limit stops the search early.
type GrepReply struct {
	Matches          []GrepMatch `json:"matches"`
	TotalMatches     int         `json:"total_matches"`      // matches found, or matching files in filesWithMatches mode
	FilesWithMatches int         `json:"files_with_matches"` // searched files containing at least one match
	FilesSearched    int         `json:"files_searched"`     // files read and searched, including binary files
	LimitReached     bool        `json:"limit_reached"`      // a result or per-file cap left out possible matches
	SkippedOversized int         `json:"skipped_oversized"`  // files over MaxFileSize that could not be read; readable ones are searched over that prefix
}

// GlobArgs lists paths matching Pattern below the selected root.
type GlobArgs struct {
	SearchArgs
	Pattern     string `json:"pattern"`                 // glob over slash-separated root-relative paths
	Anchored    bool   `json:"anchored,omitempty"`      // match slash-free patterns only at the root
	Type        string `json:"type,omitempty"`          // empty (all), file, dir, or symlink; file/dir can resolve symlink targets
	Limit       int    `json:"limit,omitempty"`         // maximum returned matches; zero means unlimited
	SortByMtime bool   `json:"sort_by_mtime,omitempty"` // newest first, ties by path; also request metadata
}

// GlobMatch is a matching path, with metadata when requested by SortByMtime.
type GlobMatch struct {
	Path  string `json:"path"`  // slash-separated path relative to the search root, without a trailing slash
	Type  string `json:"type"`  // file, dir, or symlink
	Size  int64  `json:"size"`  // regular-file byte size, or -1 when absent
	Mtime int64  `json:"mtime"` // modification time in Unix nanoseconds, or zero when absent
}

// GlobReply contains matches in path order, or newest-first with SortByMtime.
type GlobReply struct {
	Matches   []GlobMatch `json:"matches"`
	Total     int         `json:"total"`     // matching paths before applying Limit
	Truncated bool        `json:"truncated"` // Limit dropped matches
}
