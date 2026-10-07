package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"grimalkin/internal/core"
)

var searchSpec = core.ToolSpec{
	Name:        "search",
	Description: "Search the current project by meaning. Shortlists files locally by keyword, then sends bounded excerpts to Jev (TypeSafe) to rank their relevance, and returns file paths and line ranges to read with shell. The shortlist decides what can be found, so give terms: likely identifiers, file and folder names, and words for the parts of the feature you expect (for 'how does search work': index, reindex, facet, SearchRepo, the route, the client that calls it). Terms add to the query's own words. glob limits files (e.g. **/*.go). Results are a shortlist, not exhaustive. If you are not getting what you need, fall back to ripgrep and the shell: search can miss files, especially indirect dependencies and call chains.",
	InputSchema: `{"type":"object","properties":{"query":{"type":"string","description":"What behaviour or implementation are you looking for?"},"terms":{"type":"array","items":{"type":"string"},"description":"Literal, case-insensitive keywords added to the query's own words to shortlist files: identifiers, file or folder names, related concepts. Up to 12."},"glob":{"type":"string","description":"Optional ripgrep file glob, e.g. **/*.go or internal/**."}},"required":["query"],"additionalProperties":false}`,
}

// Variables, not constants, so offline evaluations can widen them.
var (
	searchTimeout = 45 * time.Second
	searchFiles   = 60
	searchBytes   = 384 * 1024
	searchWorkers = 8
)

type Search struct {
	root     string
	key      string
	model    string
	endpoint string
}

// NewSearch makes search available in every project, without a repo allowlist.
// Credentials come from the application config, never from repository files.
func NewSearch(root, key, model string) *Search {
	return &Search{root: root, key: key, model: model, endpoint: "https://api.typesafe.ai/v1/systemone"}
}

func (*Search) Spec() core.ToolSpec { return searchSpec }
func (*Search) Summary(input json.RawMessage) string {
	var in searchInput
	if json.Unmarshal(input, &in) != nil {
		return "search"
	}
	return "search " + in.Query
}

type searchInput struct {
	Query string   `json:"query"`
	Terms []string `json:"terms"`
	Glob  string   `json:"glob"`
}

var searchWord = regexp.MustCompile(`[a-zA-Z0-9_]+`)

// maxSearchTerms caps the model's terms plus the query's words.
const maxSearchTerms = 20

func parseSearch(input json.RawMessage) (searchInput, error) {
	var in searchInput
	dec := json.NewDecoder(strings.NewReader(string(input)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, errors.New("invalid search input: expected query, optional terms and glob")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return in, errors.New("invalid search input: trailing JSON")
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" || len(in.Query) > 2000 || len(in.Glob) > 500 || len(in.Terms) > 12 {
		return in, errors.New("search requires a query (1–2000 bytes), up to 12 terms, and a glob of at most 500 bytes")
	}
	// The model's terms come first, then the query's own words: terms add to
	// the query rather than replace it, so a thin term list can't shrink the
	// shortlist to what one grep would find.
	given := in.Terms
	in.Terms = nil
	seen := map[string]bool{}
	for _, term := range given {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" || len(term) > 100 {
			return in, errors.New("search terms must be non-empty and at most 100 bytes")
		}
		if !seen[term] {
			in.Terms = append(in.Terms, term)
			seen[term] = true
		}
	}
	stop := " a an and are as at be been by can code do does file files find for from has how i in into is it me of on or that the their this to what when where which with would "
	for _, term := range searchWord.FindAllString(strings.ToLower(in.Query), -1) {
		if len(in.Terms) == maxSearchTerms {
			break
		}
		if len(term) < 3 || strings.Contains(stop, " "+term+" ") || seen[term] {
			continue
		}
		in.Terms = append(in.Terms, term)
		seen[term] = true
	}
	if len(in.Terms) == 0 {
		return in, errors.New("no search keywords; supply terms or use ripgrep via shell")
	}
	return in, nil
}

type searchChunk struct {
	Path        string
	First, Last int
	Content     string
	Lexical     float64
	Score       float64
}

type searchStats struct {
	Listed, Read, Skipped int
	Limited               bool
}

func (s *Search) Run(ctx context.Context, input json.RawMessage) (string, error) {
	in, err := parseSearch(input)
	if err != nil {
		return "", err
	}
	if s.key == "" || s.model == "" {
		return "", errors.New("search is missing Jev credentials; configure search.apiKey or use ripgrep via shell")
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	start := time.Now()
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return "", fmt.Errorf("search: open project: %w", err)
	}
	defer root.Close()
	candidates, stats, err := searchCandidates(ctx, s.root, root, in)
	if err != nil {
		return "", fmt.Errorf("search: %w; fall back to ripgrep via shell", err)
	}
	if len(candidates) == 0 {
		return fmt.Sprintf("No keyword candidates for %q (%d files inspected; %d skipped). This is not proof that no relevant code exists. Broaden terms or glob, or fall back to ripgrep and shell.", in.Query, stats.Read, stats.Skipped), nil
	}
	// Preserve a few lexical hits so semantic ranking cannot hide every keyword hit.
	lexical := append([]searchChunk(nil), candidates...)
	usage, rankErr := s.rank(ctx, in.Query, candidates)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if rankErr == nil {
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Search: %s\nKeyword shortlist: %s\n", in.Query, strings.Join(in.Terms, ", "))
	if rankErr != nil {
		fmt.Fprintf(&out, "Jev ranking unavailable: %v. Showing local keyword matches only; use ripgrep and shell if these are insufficient.\n", rankErr)
	}
	// At most ten semantic files plus five lexical files, one best range per file.
	seen := map[string]bool{}
	add := func(c searchChunk, local bool) {
		if seen[c.Path] {
			return
		}
		seen[c.Path] = true
		if local {
			fmt.Fprintf(&out, "%s:%d-%d [keyword match]\n", c.Path, c.First, c.Last)
		} else {
			fmt.Fprintf(&out, "%s:%d-%d [Jev relevance %.2f]\n", c.Path, c.First, c.Last, c.Score)
		}
	}
	for _, c := range candidates {
		if len(seen) == 10 {
			break
		}
		add(c, rankErr != nil)
	}
	localFiles := map[string]bool{}
	for _, c := range lexical {
		if localFiles[c.Path] {
			continue
		}
		localFiles[c.Path] = true
		add(c, true)
		if len(localFiles) == 5 {
			break
		}
	}
	fmt.Fprintf(&out, "\n%d files inspected; %d skipped; %d excerpts selected. Elapsed %.2fs.\n", stats.Read, stats.Skipped, len(candidates), time.Since(start).Seconds())
	if rankErr == nil {
		fmt.Fprintf(&out, "Jev: %d input tokens, %d output tokens; estimated $%.5f at $0.042/M input tokens (output free; verify your model's pricing).\n", usage.Input, usage.Output, float64(usage.Input)*0.042/1e6)
	} else {
		out.WriteString("Partial/failed requests may have incurred charges; complete token usage unavailable.\n")
	}
	if stats.Limited {
		out.WriteString("Local scan reached its file/byte limit; narrow glob or use ripgrep via shell.\n")
	}
	out.WriteString("Only a bounded keyword shortlist was sent, not the whole repository. Read the listed ranges with shell. If this misses what you need, fall back to ripgrep and shell.")
	return out.String(), nil
}
