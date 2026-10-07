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
	mapOn    bool
	git      bool // root is inside a git work tree
	repoMap  searchMap
}

// NewSearch makes search available in every project, without a repo allowlist.
// Credentials come from the application config, never from repository files.
// With repoMap, the repo map is built in the background (see search_map.go):
// in a git repository by StartMap or the first search, elsewhere only by
// Index.
func NewSearch(root, key, model string, repoMap bool) *Search {
	return &Search{root: root, key: key, model: model, endpoint: "https://api.typesafe.ai/v1/systemone", mapOn: repoMap, git: searchInGitRepo(root)}
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
	Via         string // the map's concepts, when the map put it on the shortlist
	MapOnly     bool   // no keyword in the file: only the map put it there
}

type searchStats struct {
	Listed, Read, Skipped int
	Limited               bool
}

// searchRefineChunks caps the extra excerpts scored in the second pass. A
// variable so tests can shrink it.
var searchRefineChunks = 40

// refine is a second pass within the shortlisted files. Keywords chose one
// excerpt of each for Jev; a large file's answer can sit in another. The
// shortlisted files' other excerpts with the most keywords are scored too,
// and each file moves to its best excerpt, so a file whose first excerpt
// scored low can still rise. A second excerpt scoring at least 0.5 goes in
// also. On error the candidates keep their first scores. It returns how many
// excerpts it sent.
func (s *Search) refine(ctx context.Context, query string, candidates []searchChunk, fileChunks map[string][]searchChunk, also map[string]searchChunk) (int, error) {
	index := map[string]int{}
	for i, c := range candidates {
		index[c.Path] = i
	}
	var extra []searchChunk
	for i, c := range candidates {
		for _, chunk := range fileChunks[c.Path] {
			if chunk.First != candidates[i].First && chunk.Lexical > 0 {
				extra = append(extra, chunk)
			}
		}
	}
	sort.SliceStable(extra, func(i, j int) bool { return extra[i].Lexical > extra[j].Lexical })
	if len(extra) > searchRefineChunks {
		extra = extra[:searchRefineChunks]
	}
	if len(extra) == 0 {
		return 0, nil
	}
	if _, err := s.rank(ctx, query, extra); err != nil {
		return len(extra), err
	}
	second := map[string]searchChunk{}
	for _, e := range extra {
		c := &candidates[index[e.Path]]
		e.Via = c.Via
		switch {
		case e.Score > c.Score:
			second[e.Path] = *c
			*c = e
		case e.Score > second[e.Path].Score:
			second[e.Path] = e
		}
	}
	for path, e := range second {
		if e.Score >= 0.5 {
			also[path] = e
		}
	}
	return len(extra), nil
}

func (s *Search) Run(ctx context.Context, input json.RawMessage) (string, error) {
	in, err := parseSearch(input)
	if err != nil {
		return "", err
	}
	if s.key == "" || s.model == "" {
		return "", errors.New("search is missing Jev credentials; configure search.apiKey or use ripgrep via shell")
	}
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	start := time.Now()
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return "", fmt.Errorf("search: open project: %w", err)
	}
	defer root.Close()
	var mapScores map[string]searchMapScore
	var roles map[string]string
	var mapErr error
	var mapStatus string
	if s.mapOn {
		s.StartMap()
		// Read before the lookup, so it says what this search used even if
		// the map finishes while it runs.
		mapStatus = s.mapStatus()
		mapScores, roles, _, mapErr = s.mapScores(ctx, in)
	}
	candidates, fileChunks, stats, err := searchCandidates(ctx, s.root, root, in, mapScores)
	if err != nil {
		return "", fmt.Errorf("search: %w; fall back to ripgrep via shell", err)
	}
	if len(candidates) == 0 {
		return fmt.Sprintf("No keyword candidates for %q (%d files inspected; %d skipped). This is not proof that no relevant code exists. Broaden terms or glob, or fall back to ripgrep and shell.", in.Query, stats.Read, stats.Skipped), nil
	}
	// Preserve a few lexical hits so semantic ranking cannot hide every keyword hit.
	lexical := append([]searchChunk(nil), candidates...)
	_, rankErr := s.rank(ctx, in.Query, candidates)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	also := map[string]searchChunk{}
	refined := 0
	var refineErr error
	if rankErr == nil {
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
		// Keywords chose which excerpt of each file Jev saw. Score other
		// excerpts of the shortlisted files too, so the range shown is the
		// part that answers the query, not the part with the most keywords.
		// The first ranking stands if this pass fails or runs out of time.
		refined, refineErr = s.refine(ctx, in.Query, candidates, fileChunks, also)
		if caller.Err() != nil {
			return "", caller.Err()
		}
		if refineErr == nil {
			sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Search: %s\nKeyword shortlist: %s\n", in.Query, strings.Join(in.Terms, ", "))
	if s.mapOn {
		out.WriteString(mapStatus + "\n")
		if mapErr != nil {
			fmt.Fprintf(&out, "Repo map lookup failed: %v; keywords only.\n", mapErr)
		}
	}
	if rankErr != nil {
		fmt.Fprintf(&out, "Jev ranking unavailable: %v. Showing local keyword matches only; use ripgrep and shell if these are insufficient.\n", rankErr)
	}
	if refineErr != nil {
		fmt.Fprintf(&out, "Second pass unavailable: %v. Ranges and scores are from the first pass.\n", refineErr)
	}
	// At most ten semantic files, five more the map found and five lexical
	// files, one best range per file.
	seen := map[string]bool{}
	add := func(c searchChunk, local bool) {
		if seen[c.Path] {
			return
		}
		seen[c.Path] = true
		note := "keyword match"
		switch {
		case !local:
			note = fmt.Sprintf("Jev relevance %.2f", c.Score)
		case c.MapOnly:
			note = "map match"
		}
		if role := roles[c.Path]; role != "" {
			note += "; " + role
		}
		if c.Via != "" {
			note += "; via map: " + c.Via
		}
		if a, ok := also[c.Path]; ok && !local {
			note += fmt.Sprintf("; also %d-%d at %.2f", a.First, a.Last, a.Score)
		}
		fmt.Fprintf(&out, "%s:%d-%d [%s]\n", c.Path, c.First, c.Last, note)
	}
	for _, c := range candidates {
		if len(seen) == 10 {
			break
		}
		add(c, rankErr != nil)
	}
	// Files only the map found rarely beat the keyword picks on relevance,
	// so the best few get their own place rather than vanishing below the
	// top ten.
	if rankErr == nil {
		viaMap := 0
		for _, c := range candidates {
			if viaMap == 5 || c.Score < 0.5 {
				break
			}
			if c.Via != "" && !seen[c.Path] {
				add(c, false)
				viaMap++
			}
		}
	}
	localFiles := map[string]bool{}
	for _, c := range lexical {
		if localFiles[c.Path] || c.MapOnly {
			continue
		}
		localFiles[c.Path] = true
		add(c, true)
		if len(localFiles) == 5 {
			break
		}
	}
	fmt.Fprintf(&out, "\n%d files inspected; %d skipped; %d excerpts sent, and %d more in the second pass. Elapsed %.2fs.\n", stats.Read, stats.Skipped, len(candidates), refined, time.Since(start).Seconds())
	if stats.Limited {
		out.WriteString("Local scan reached its file/byte limit; narrow glob or use ripgrep via shell.\n")
	}
	out.WriteString("Only a bounded shortlist was sent for this search, not the whole repository. Read the listed ranges with shell. If this misses what you need, fall back to ripgrep and shell.")
	return out.String(), nil
}
