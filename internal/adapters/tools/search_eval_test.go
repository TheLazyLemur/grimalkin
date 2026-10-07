package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestSearchShortlistRecall is an offline evaluation, skipped by default: for
// each labelled question, does every file that matters reach Jev? It calls no
// API. Run with
//
//	SEARCH_EVAL='<repo>=<benchmark.json>;…' go test ./internal/adapters/tools -run ShortlistRecall -v
//
// using example/benchmark.json or labelled questions about other repos, which
// stay outside this repository.
func TestSearchShortlistRecall(t *testing.T) {
	sets := os.Getenv("SEARCH_EVAL")
	if sets == "" {
		t.Skip("SEARCH_EVAL not set")
	}
	total, hit := 0, 0
	for _, pair := range strings.Split(sets, ";") {
		dir, benchPath, _ := strings.Cut(pair, "=")
		data, err := os.ReadFile(benchPath)
		if err != nil {
			t.Fatal(err)
		}
		var bench struct {
			Queries []struct {
				ID, Question string
				Must         []string
			}
		}
		if err := json.Unmarshal(data, &bench); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range bench.Queries {
			input, _ := json.Marshal(map[string]string{"query": q.Question})
			in, err := parseSearch(input)
			if err != nil {
				t.Fatal(q.ID, err)
			}
			candidates, _, _, err := searchCandidates(context.Background(), dir, root, in, nil)
			if err != nil {
				t.Fatal(err)
			}
			sent := map[string]bool{}
			for _, c := range candidates {
				sent[c.Path] = true
			}
			var missed []string
			for _, path := range q.Must {
				total++
				if sent[path] {
					hit++
				} else {
					missed = append(missed, path)
				}
			}
			fmt.Printf("%-22s %2d files, missed %v\n", q.ID, len(sent), missed)
		}
		root.Close()
	}
	fmt.Printf("shortlist recall %d/%d\n", hit, total)
}
