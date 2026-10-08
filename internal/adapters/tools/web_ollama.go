package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const (
	ollamaWebEndpoint = "https://ollama.com"
	maxOllamaWebBytes = 8 * 1024 * 1024
)

// OllamaWeb is the WebProvider over Ollama's hosted web search and fetch.
// Its key is an ollama.com account's, not a local server's.
type OllamaWeb struct {
	key      string
	endpoint string
}

func NewOllamaWeb(key string) *OllamaWeb {
	if key == "" {
		panic("ollama web needs a key")
	}
	return &OllamaWeb{key: key, endpoint: ollamaWebEndpoint}
}

func (o *OllamaWeb) Search(ctx context.Context, query string, limit int) ([]WebResult, error) {
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := o.post(ctx, "/api/web_search", map[string]any{"query": query, "max_results": limit}, &out); err != nil {
		return nil, err
	}
	results := make([]WebResult, len(out.Results))
	for i, r := range out.Results {
		results[i] = WebResult{Title: r.Title, URL: r.URL, Content: r.Content}
	}
	return results, nil
}

func (o *OllamaWeb) Fetch(ctx context.Context, url string) (WebPage, error) {
	var out struct {
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Links   []string `json:"links"`
	}
	if err := o.post(ctx, "/api/web_fetch", map[string]string{"url": url}, &out); err != nil {
		return WebPage{}, err
	}
	return WebPage{Title: out.Title, Content: out.Content, Links: out.Links}, nil
}

// post sends one request and decodes the answer. Its errors are in
// grimalkin's own words: a response body is never echoed, since the model
// reads the error.
func (o *OllamaWeb) post(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := searchHTTPClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("ollama: request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: HTTP %d", resp.StatusCode)
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxOllamaWebBytes+1))
	if err != nil || len(answer) > maxOllamaWebBytes || json.Unmarshal(answer, out) != nil {
		return errors.New("ollama: unreadable response")
	}
	return nil
}
