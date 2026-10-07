package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJev scores an excerpt 0.9 when it contains "winner", else 0.1, and
// records every path it was sent.
type fakeJev struct {
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
	status []int // statuses to answer with before succeeding
}

func (f *fakeJev) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	f.mu.Lock()
	if len(f.status) > 0 {
		status := f.status[0]
		f.status = f.status[1:]
		f.mu.Unlock()
		http.Error(w, "echoed secret sk-should-not-appear", status)
		return
	}
	state := body["state"].(map[string]any)
	f.paths = append(f.paths, state["path"].(string))
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-key" {
		http.Error(w, "unauthorised", 401)
		return
	}
	score := 0.1
	if strings.Contains(state["content"].(string), "winner") {
		score = 0.9
	}
	json.NewEncoder(w).Encode(map[string]any{
		"model":   "jev-test",
		"answers": map[string]any{"relevant": map[string]any{"type": "noul", "noul": score}},
		"usage":   map[string]any{"input_tokens": 100, "output_tokens": 1},
	})
}

func (f *fakeJev) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := slices.Clone(f.paths)
	slices.Sort(paths)
	return slices.Compact(paths)
}

func project(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func searchWith(t *testing.T, dir string, jev http.Handler) *Search {
	t.Helper()
	server := httptest.NewServer(jev)
	t.Cleanup(server.Close)
	s := NewSearch(dir, "test-key", "jev-test")
	s.endpoint = server.URL
	return s
}

func searchInputJSON(query string, terms []string, glob string) json.RawMessage {
	in := map[string]any{"query": query}
	if terms != nil {
		in["terms"] = terms
	}
	if glob != "" {
		in["glob"] = glob
	}
	data, _ := json.Marshal(in)
	return data
}

func TestSearchRanksKeywordCandidatesByJevRelevance(t *testing.T) {
	// given
	// ... two files matching the keyword, one of which Jev scores higher
	dir := project(t, map[string]string{
		"a_cancel.go": "package a\n// cancel the thing\n",
		"b_cancel.go": "package b\n// cancel: the winner\n",
		"other.go":    "package c\n",
	})
	jev := &fakeJev{}
	s := searchWith(t, dir, jev)

	// when
	// ... the model searches
	out, err := s.Run(context.Background(), searchInputJSON("where is cancel handled", []string{"cancel"}, ""))

	// then
	// ... the higher-scored file is listed first, with its line range and the fallback note
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Index(out, "b_cancel.go:1-2 [Jev relevance 0.90]")
	second := strings.Index(out, "a_cancel.go:1-2 [Jev relevance 0.10]")
	if first < 0 || second < 0 || first > second {
		t.Errorf("want b_cancel.go ranked above a_cancel.go, got:\n%s", out)
	}
	if !strings.Contains(out, "fall back to ripgrep and shell") {
		t.Errorf("output lacks the fallback note:\n%s", out)
	}
	// ... and only keyword matches were sent, with the model and the question
	if got := jev.sent(); !slices.Equal(got, []string{"a_cancel.go", "b_cancel.go"}) {
		t.Errorf("sent %v, want only the two keyword matches", got)
	}
	body := jev.bodies[0]
	question := body["questions"].(map[string]any)["relevant"].(map[string]any)
	task := question["instructions"].(map[string]any)["task"]
	if body["model"] != "jev-test" || task != "where is cancel handled" {
		t.Errorf("model, task = %v, %v", body["model"], task)
	}
}

func TestSearchNeverSendsIgnoredHiddenSecretOrOutsideFiles(t *testing.T) {
	// given
	// ... keyword matches that are gitignored, hidden, hold a private key, or
	// ... reach outside the project through a symlink
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("token outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := project(t, map[string]string{
		".gitignore":      "ignored.go\n",
		"ignored.go":      "token ignored\n",
		".env":            "token=hidden\n",
		".config/a.go":    "token hidden dir\n",
		"key.go":          "token\n-----BEGIN RSA PRIVATE KEY-----\n",
		"secrets/conf.go": "token in secrets dir\n",
		"kept.go":         "token kept\n",
	})
	if err := os.Symlink(outside, filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	jev := &fakeJev{}
	s := searchWith(t, dir, jev)

	// when
	// ... the model searches for the keyword they all share
	_, err := s.Run(context.Background(), searchInputJSON("token handling", []string{"token"}, ""))

	// then
	// ... only the ordinary project file is sent
	if err != nil {
		t.Fatal(err)
	}
	if got := jev.sent(); !slices.Equal(got, []string{"kept.go"}) {
		t.Errorf("sent %v, want only kept.go", got)
	}
}

func TestSearchGlobLimitsTheFilesConsidered(t *testing.T) {
	// given
	// ... matches inside and outside the globbed directory
	dir := project(t, map[string]string{
		"internal/in.go": "retry here\n",
		"cmd/out.go":     "retry there\n",
	})
	jev := &fakeJev{}
	s := searchWith(t, dir, jev)

	// when
	// ... the model limits the search to internal/
	_, err := s.Run(context.Background(), searchInputJSON("retry", []string{"retry"}, "internal/**"))

	// then
	// ... only the file under internal/ is sent
	if err != nil {
		t.Fatal(err)
	}
	if got := jev.sent(); !slices.Equal(got, []string{"internal/in.go"}) {
		t.Errorf("sent %v, want only internal/in.go", got)
	}
}

func TestSearchGlobDoesNotOverrideGitignore(t *testing.T) {
	// given
	// ... a gitignored dependency directory inside the globbed path
	dir := project(t, map[string]string{
		".gitignore":              "node_modules/\n",
		"web/app.ts":              "retry here\n",
		"web/node_modules/dep.ts": "retry in a dependency\n",
	})
	jev := &fakeJev{}
	s := searchWith(t, dir, jev)

	// when
	// ... the model globs the directory
	_, err := s.Run(context.Background(), searchInputJSON("retry", []string{"retry"}, "web/**"))

	// then
	// ... the ignored file stays out
	if err != nil {
		t.Fatal(err)
	}
	if got := jev.sent(); !slices.Equal(got, []string{"web/app.ts"}) {
		t.Errorf("sent %v, want only web/app.ts", got)
	}
}

func TestSearchGlobMatchesLikeRipgrep(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"*.go", "a.go", true},
		{"*.go", "internal/a.go", true},
		{"*.go", "a.gox", false},
		{"internal/**", "internal/x/y.go", true},
		{"internal/**", "cmd/internal/y.go", false},
		{"**/*_test.go", "a_test.go", true},
		{"**/*_test.go", "x/y/a_test.go", true},
		{"src/*.ts", "src/a/b.ts", false},
		{"a?c.go", "abc.go", true},
		{"a.b", "axb", false},
	}
	for _, tc := range cases {
		if got := searchGlob(tc.glob).MatchString(tc.path); got != tc.want {
			t.Errorf("glob %q on %q = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

func TestSearchTermsDefaultToTheQuerysKeywords(t *testing.T) {
	// given
	// ... a file matching a word from the query, and one matching only a stop word
	dir := project(t, map[string]string{
		"clip.go":  "clipboard copy\n",
		"where.go": "where where where\n",
	})
	jev := &fakeJev{}
	s := searchWith(t, dir, jev)

	// when
	// ... the model gives no terms
	out, err := s.Run(context.Background(), searchInputJSON("where is the clipboard written", nil, ""))

	// then
	// ... keywords come from the query without its stop words
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Keyword shortlist: clipboard, written") {
		t.Errorf("unexpected keywords:\n%s", out)
	}
	if got := jev.sent(); !slices.Equal(got, []string{"clip.go"}) {
		t.Errorf("sent %v, want only clip.go", got)
	}
}

func TestSearchTermsAddToTheQuerysKeywords(t *testing.T) {
	// given
	// ... a file found only by a query word, and one found only by a term
	dir := project(t, map[string]string{
		"jobs/reindex.go": "reindex every five minutes\n",
		"web/client.go":   "fetch the catalog\n",
	})
	jev := &fakeJev{}
	s := searchWith(t, dir, jev)

	// when
	// ... the model gives a term, repeated in another case, beside its query
	out, err := s.Run(context.Background(), searchInputJSON("how does reindex work", []string{"catalog", "Catalog"}, ""))

	// then
	// ... both files are shortlisted, and the term appears once, first
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Keyword shortlist: catalog, reindex, work\n") {
		t.Errorf("unexpected keywords:\n%s", out)
	}
	if got := jev.sent(); !slices.Equal(got, []string{"jobs/reindex.go", "web/client.go"}) {
		t.Errorf("sent %v, want both files", got)
	}
}

func TestSearchRetriesWhenRateLimited(t *testing.T) {
	// given
	// ... Jev rate-limits the first request
	defer func(retries int, backoff time.Duration) { searchRetries, searchBackoff = retries, backoff }(searchRetries, searchBackoff)
	searchBackoff = time.Millisecond
	dir := project(t, map[string]string{"a.go": "queue winner\n"})
	jev := &fakeJev{status: []int{http.StatusTooManyRequests}}
	s := searchWith(t, dir, jev)

	// when
	// ... the model searches
	out, err := s.Run(context.Background(), searchInputJSON("queue", []string{"queue"}, ""))

	// then
	// ... the retry succeeds and the file is ranked
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:1-1 [Jev relevance 0.90]") {
		t.Errorf("want a ranked result after the retry:\n%s", out)
	}
}

func TestSearchFallsBackToKeywordMatchesWhenJevFails(t *testing.T) {
	// given
	// ... Jev refuses the key, with a body that must not reach the model
	dir := project(t, map[string]string{"a.go": "queue\n"})
	jev := &fakeJev{status: []int{401}}
	s := searchWith(t, dir, jev)

	// when
	// ... the model searches
	out, err := s.Run(context.Background(), searchInputJSON("queue", []string{"queue"}, ""))

	// then
	// ... it gets the keyword matches and the reason, but not the body
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Jev ranking unavailable: Jev HTTP 401", "a.go:1-1 [keyword match]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk-should-not-appear") {
		t.Errorf("output echoes Jev's error body:\n%s", out)
	}
}

func TestSearchRejectsAnInvalidJevScore(t *testing.T) {
	// given
	// ... Jev answers with a score outside 0..1
	dir := project(t, map[string]string{"a.go": "queue\n"})
	s := searchWith(t, dir, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"relevant":{"type":"noul","noul":7}},"usage":{"input_tokens":1,"output_tokens":0}}`))
	}))

	// when
	// ... the model searches
	out, err := s.Run(context.Background(), searchInputJSON("queue", []string{"queue"}, ""))

	// then
	// ... the score is not trusted; keyword matches are shown instead
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "invalid Jev score or usage") || !strings.Contains(out, "[keyword match]") {
		t.Errorf("want a keyword fallback naming the invalid score:\n%s", out)
	}
}

func TestSearchWithNoCandidatesSaysSoWithoutCallingJev(t *testing.T) {
	// given
	// ... a project with no keyword match
	dir := project(t, map[string]string{"a.go": "package a\n"})
	jev := &fakeJev{}
	s := searchWith(t, dir, jev)

	// when
	// ... the model searches
	out, err := s.Run(context.Background(), searchInputJSON("zebra", []string{"zebra"}, ""))

	// then
	// ... it is told nothing matched and to fall back, and Jev is never called
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No keyword candidates") || !strings.Contains(out, "fall back to ripgrep") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if len(jev.sent()) != 0 {
		t.Errorf("Jev was called for %v", jev.sent())
	}
}

func TestSearchRejectsInvalidInput(t *testing.T) {
	s := NewSearch(t.TempDir(), "k", "m")
	for _, input := range []string{`{}`, `{"query":"  "}`, `{"query":"x","extra":1}`, `{"query":"x","terms":[""]}`, `{"query":"is it"}`} {
		if _, err := s.Run(context.Background(), json.RawMessage(input)); err == nil {
			t.Errorf("input %s: want an error", input)
		}
	}
}

func TestShortlistFavoursRareTermsPathsAndCodeOverTests(t *testing.T) {
	// given
	// ... a repository where "api" is everywhere, a test repeats "search", and
	// ... the implementation lives under search/
	file := func(name, content string) searchFile {
		return searchFile{name: name, chunks: splitSearchFile(name, content)}
	}
	files := []searchFile{
		file("search/repo.ts", "export function find(q) { return db.where(q) }\n"),
		file("search/repo.test.ts", "search search search search api\n"),
		file("billing/charges.ts", "api api api text api full text\n"),
	}
	for i := range 20 {
		files = append(files, file(fmt.Sprintf("misc/f%d.ts", i), "api text\n"))
	}

	// when
	// ... the shortlist is chosen for a query mixing a rare and generic terms
	selected := selectSearchChunks(files, searchInput{Terms: []string{"search", "api", "text"}})

	// then
	// ... the implementation ranks first and the test below it
	rank := map[string]int{}
	for i, c := range selected {
		rank[c.Path] = i + 1
	}
	if rank["search/repo.ts"] != 1 || rank["search/repo.test.ts"] <= rank["search/repo.ts"] {
		t.Errorf("ranks = %v, want search/repo.ts first and its test below it", rank)
	}
}

func TestSearchStemFindsInflections(t *testing.T) {
	for term, want := range map[string]string{"filtering": "filter", "bookings": "booking", "cached": "cach", "api": "api", "uses": "uses"} {
		if got := searchStem(term); got != want {
			t.Errorf("searchStem(%q) = %q, want %q", term, got, want)
		}
	}
}

func TestSplitSearchFileKeepsExactLineRanges(t *testing.T) {
	// given
	// ... lines that overflow one chunk, then a single line too long to send
	line := strings.Repeat("x", searchChunkBytes/2-1) + "\n" // two fill a chunk exactly
	giant := strings.Repeat("y", searchChunkBytes+1) + "\n"
	text := line + line + line + giant + "tail\n"

	// when
	// ... the file is split
	chunks := splitSearchFile("f.go", text)

	// then
	// ... chunks hold whole lines with exact ranges, and the giant line is left out
	var got [][2]int
	for _, c := range chunks {
		got = append(got, [2]int{c.First, c.Last})
		if len(c.Content) > searchChunkBytes {
			t.Errorf("chunk %d-%d is %d bytes", c.First, c.Last, len(c.Content))
		}
	}
	want := [][2]int{{1, 2}, {3, 3}, {5, 5}}
	if !slices.Equal(got, want) {
		t.Errorf("ranges = %v, want %v", got, want)
	}
}
