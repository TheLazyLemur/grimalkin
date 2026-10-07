package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxSearchPaths     = 10_000
	maxSearchScanBytes = 16 * 1024 * 1024
	maxSearchFileBytes = 256 * 1024
	searchChunkBytes   = 8 * 1024
)

var sensitiveSearchContent = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----|AKIA[0-9A-Z]{16}|sk-ant-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{30,}|xox[baprs]-[A-Za-z0-9-]{20,}|(?i:["']?(?:api[_-]?key|access[_-]?token|client[_-]?secret|password)["']?\s*[:=]\s*["'][^"'\r\n]{12,}["'])`)

func searchSafePath(name string) bool {
	if path.IsAbs(name) || path.Clean(name) != name || strings.HasPrefix(name, "../") {
		return false
	}
	parts := strings.Split(strings.ToLower(name), "/")
	for _, p := range parts {
		switch p {
		case "node_modules", "vendor", "generated", "dist", "build", "bin", "obj", "coverage", "__pycache__", "credentials", "secrets":
			return false
		}
		if strings.HasPrefix(p, ".") {
			return false
		}
	}
	base := parts[len(parts)-1]
	if strings.Contains(base, "credential") || strings.Contains(base, "secret") || strings.Contains(base, "lock") && (strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".yaml")) {
		return false
	}
	switch base {
	case "makefile", "dockerfile", "go.mod":
		return true
	}
	switch path.Ext(base) {
	case ".go", ".py", ".rs", ".ts", ".tsx", ".js", ".jsx", ".cs", ".java", ".c", ".cpp", ".h", ".hpp", ".rb", ".php", ".sh", ".sql", ".md", ".txt", ".yaml", ".yml", ".toml", ".json", ".xml", ".css", ".scss", ".html", ".vue", ".svelte", ".kt", ".swift", ".graphql":
		return !strings.HasSuffix(base, ".min.js") && !strings.HasSuffix(base, ".d.ts")
	}
	return false
}

// Capped output prevents huge file lists from filling memory. We discard the
// incomplete final path when enumeration is cut short.
type searchPathOutput struct {
	bytes.Buffer
	limited bool
}

func (b *searchPathOutput) Write(p []byte) (int, error) {
	const max = 2 * 1024 * 1024
	if len(p) > max-b.Len() {
		n, _ := b.Buffer.Write(p[:max-b.Len()])
		b.limited = true
		return n, errors.New("search file list limit reached")
	}
	return b.Buffer.Write(p)
}

func searchCandidates(ctx context.Context, cwd string, root *os.Root, in searchInput) ([]searchChunk, searchStats, error) {
	stats := searchStats{}
	// The glob is applied here rather than passed to rg: an rg --glob
	// overrides .gitignore, which would pull in node_modules and build output.
	var glob *regexp.Regexp
	if in.Glob != "" {
		glob = searchGlob(in.Glob)
	}
	args := []string{"--no-config", "--files", "--null", "--", "."}
	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = cwd
	var output searchPathOutput
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, stats, ctx.Err()
	}
	var exitErr *exec.ExitError
	if err != nil && !output.limited && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
		if stderr.Len() > 0 {
			return nil, stats, fmt.Errorf("ripgrep file listing: %s", strings.TrimSpace(stderr.String()))
		}
		return nil, stats, fmt.Errorf("ripgrep file listing: %w", err)
	}
	data := output.Bytes()
	end := bytes.LastIndexByte(data, 0)
	if end < 0 {
		return nil, stats, nil
	}
	names := strings.Split(string(data[:end]), "\x00")
	sort.Strings(names)
	stats.Limited = output.limited
	if len(names) > maxSearchPaths {
		names = names[:maxSearchPaths]
		stats.Limited = true
	}
	stats.Listed = len(names)
	var read []searchFile
	totalRead := 0
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, stats, err
		}
		name = strings.TrimPrefix(name, "./")
		if glob != nil && !glob.MatchString(name) {
			continue
		}
		if !searchSafePath(name) {
			stats.Skipped++
			continue
		}
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxSearchFileBytes {
			stats.Skipped++
			continue
		}
		if totalRead+int(info.Size()) > maxSearchScanBytes {
			stats.Limited = true
			break
		}
		// os.Root prevents paths and symlinks from escaping the project. Nonregular
		// files and final symlinks are excluded above; read size is bounded as well.
		f, err := root.Open(name)
		if err != nil {
			stats.Skipped++
			continue
		}
		b, readErr := io.ReadAll(io.LimitReader(f, maxSearchFileBytes+1))
		f.Close()
		totalRead += len(b)
		if readErr != nil || len(b) > maxSearchFileBytes || bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) || sensitiveSearchContent.Match(b) {
			stats.Skipped++
			continue
		}
		stats.Read++
		read = append(read, searchFile{name: name, chunks: splitSearchFile(name, string(b))})
	}
	return selectSearchChunks(read, in), stats, nil
}

type searchFile struct {
	name   string
	chunks []searchChunk
}

// selectSearchChunks scores every chunk against the terms and picks the best
// chunk of the best files, up to the file and byte budgets. Scoring is
// BM25-like: a term found in few files weighs more than one found in most
// (so "api" or "text" can't outvote "search"), repeats saturate, and a term in
// the path adds its rarity among paths. Tests and docs are discounted unless the query asks
// for them, so they can't crowd out the code that implements the behaviour.
func selectSearchChunks(files []searchFile, in searchInput) []searchChunk {
	stems := make([]string, len(in.Terms))
	for i, term := range in.Terms {
		stems[i] = searchStem(term)
	}
	lower := make([][]string, len(files))
	df := make([]int, len(stems))     // files whose content has the term
	pathDF := make([]int, len(stems)) // files whose path has it
	for i, f := range files {
		path := strings.ToLower(f.name)
		lower[i] = make([]string, len(f.chunks))
		for j, c := range f.chunks {
			lower[i][j] = strings.ToLower(c.Content)
		}
		for t, stem := range stems {
			if strings.Contains(path, stem) {
				pathDF[t]++
			}
			for _, text := range lower[i] {
				if strings.Contains(text, stem) {
					df[t]++
					break
				}
			}
		}
	}
	idf := make([]float64, len(stems))
	pathIDF := make([]float64, len(stems))
	for t := range stems {
		idf[t] = math.Log(1 + float64(len(files)+1)/float64(df[t]+1))
		pathIDF[t] = math.Log(1 + float64(len(files)+1)/float64(pathDF[t]+1))
	}
	wantTests := slices.ContainsFunc(stems, func(s string) bool { return strings.HasPrefix(s, "test") || s == "spec" })
	wantDocs := slices.ContainsFunc(stems, func(s string) bool { return strings.HasPrefix(s, "doc") || s == "readme" || s == "plan" })

	var best []searchChunk
	for i, f := range files {
		path := strings.ToLower(f.name)
		weight := 1.0
		if !wantTests && searchIsTest(path) {
			weight *= 0.4
		}
		if !wantDocs && searchIsDoc(path) {
			weight *= 0.5
		}
		if strings.Contains(path, ".generated.") || strings.Contains(path, "/migrations/") {
			weight *= 0.5
		}
		top := -1
		for j, text := range lower[i] {
			score := 0.0
			for t, stem := range stems {
				tf := float64(strings.Count(text, stem))
				score += idf[t] * tf / (tf + 1.2)
				// A path is a stronger signal than prose: a directory or file
				// named for the term is usually where it lives. Weighed by how
				// rare the term is among paths, not among contents.
				if strings.Contains(path, stem) {
					score += pathIDF[t]
				}
			}
			files[i].chunks[j].Lexical = score * weight
			if score > 0 && (top < 0 || files[i].chunks[j].Lexical > files[i].chunks[top].Lexical) {
				top = j
			}
		}
		if top >= 0 {
			best = append(best, files[i].chunks[top])
		}
	}
	sort.SliceStable(best, func(i, j int) bool { return best[i].Lexical > best[j].Lexical })
	var selected []searchChunk
	sentBytes := 0
	for _, c := range best {
		if len(selected) == searchFiles {
			break
		}
		if sentBytes+len(c.Content) > searchBytes {
			continue
		}
		selected = append(selected, c)
		sentBytes += len(c.Content)
	}
	return selected
}

// searchStem drops a common English suffix so "filtering" finds "filter".
func searchStem(term string) string {
	for _, suffix := range []string{"ing", "ed", "es", "s"} {
		if len(term)-len(suffix) >= 4 && strings.HasSuffix(term, suffix) {
			return strings.TrimSuffix(term, suffix)
		}
	}
	return term
}

func searchIsTest(path string) bool {
	base := path[strings.LastIndex(path, "/")+1:]
	return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.HasSuffix(base, "_test.go") ||
		strings.HasPrefix(base, "test_") || strings.Contains(path, "/tests/") || strings.HasPrefix(path, "tests/") ||
		strings.Contains(path, "/__tests__/") || strings.Contains(path, "test-support/") || strings.Contains(base, "tests.cs")
}

func searchIsDoc(path string) bool {
	return strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".txt") || strings.HasPrefix(path, "docs/") || strings.Contains(path, "/docs/")
}

// Whole-line chunks have exact line references. A single giant line cannot be
// sent within the budget, so it is omitted (never silently truncated and ranked).
func splitSearchFile(name, text string) []searchChunk {
	var result []searchChunk
	var content strings.Builder
	first, last := 1, 0
	flush := func() {
		if content.Len() > 0 {
			result = append(result, searchChunk{Path: name, First: first, Last: last, Content: content.String()})
		}
		content.Reset()
	}
	for i, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		if len(line) > searchChunkBytes {
			flush()
			first = i + 2
			continue
		}
		if content.Len()+len(line) > searchChunkBytes {
			flush()
			first = i + 1
		}
		if content.Len() == 0 {
			first = i + 1
		}
		content.WriteString(line)
		last = i + 1
	}
	flush()
	return result
}

// searchGlob compiles a gitignore-style glob: * and ? stay within a path
// segment, ** crosses segments, and a glob without a slash matches the base
// name anywhere, as rg's does.
func searchGlob(glob string) *regexp.Regexp {
	glob = strings.TrimPrefix(glob, "./")
	var re strings.Builder
	if !strings.Contains(glob, "/") {
		re.WriteString("(?:^|/)")
	} else {
		re.WriteString("^")
	}
	for i := 0; i < len(glob); i++ {
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			re.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			re.WriteString(".*")
			i++
		case glob[i] == '*':
			re.WriteString("[^/]*")
		case glob[i] == '?':
			re.WriteString("[^/]")
		default:
			re.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String())
}
