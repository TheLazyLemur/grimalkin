package anthropic

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"grimalkin/internal/core"
)

// tokenServer stands in for the OAuth token endpoint, answering every request
// with status and reply and recording the last request body.
func tokenServer(t *testing.T, status int, reply string) (body *map[string]string) {
	t.Helper()
	body = new(map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(body); err != nil {
			t.Errorf("token request body: %v", err)
		}
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	previous := tokenURL
	tokenURL = server.URL
	t.Cleanup(func() { tokenURL = previous })
	return body
}

func signedIn(t *testing.T, c Creds) (Store, *TokenSource) {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "creds.json"))
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	tokens, err := NewTokenSource(store)
	if err != nil {
		t.Fatal(err)
	}
	return store, tokens
}

func TestASignedInProviderSendsTheBearerTokenAndClaudeCodeFingerprint(t *testing.T) {
	// given
	// ... a provider signed in with a token far from expiry, a token endpoint that
	// ... must not be called, and an API key in the environment
	url, bodyRef, header := serve(t, toolUseSSE)
	tokenServer(t, http.StatusInternalServerError, "")
	t.Setenv("ANTHROPIC_API_KEY", "leaked-key")
	_, tokens := signedIn(t, Creds{Access: "access-1", Refresh: "refresh-1", ExpiresAt: time.Now().Add(time.Hour)})
	p := New(Config{BaseURL: url, Tokens: tokens, Model: "m", MaxTokens: 100, Thinking: "adaptive"})
	req := core.ModelRequest{System: "be brief", Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}}

	// when
	// ... the request is streamed
	_, err := p.Stream(context.Background(), req, func(string) {})

	// then
	// ... it carries the held token as a bearer, no key, the OAuth beta and CLI
	// ... identity, and the Claude Code system block ahead of grimalkin's own
	if err != nil {
		t.Fatal(err)
	}
	if got := header.Get("Authorization"); got != "Bearer access-1" {
		t.Errorf("Authorization = %q, want Bearer access-1", got)
	}
	if got := header.Get("X-Api-Key"); got != "" {
		t.Errorf("X-Api-Key = %q, want none", got)
	}
	if got := header.Get("Anthropic-Beta"); !strings.Contains(got, "oauth-2025-04-20") {
		t.Errorf("anthropic-beta = %q, want it to contain oauth-2025-04-20", got)
	}
	if got := header.Get("User-Agent"); !strings.HasPrefix(got, "claude-cli/") {
		t.Errorf("User-Agent = %q, want claude-cli/...", got)
	}
	if got := header.Get("X-Stainless-Lang"); got != "js" {
		t.Errorf("X-Stainless-Lang = %q, want js", got)
	}
	var wire wireRequest
	if err := json.Unmarshal(*bodyRef, &wire); err != nil {
		t.Fatalf("request body: %v\n%s", err, *bodyRef)
	}
	if len(wire.System) != 2 || wire.System[0].Text != claudeCodeSystemInstruction || wire.System[1].Text != "be brief" {
		t.Errorf("system = %+v", wire.System)
	}
}

func TestAStaleTokenIsRefreshedAndTheRotatedGrantSaved(t *testing.T) {
	// given
	// ... a token expiring inside the refresh skew, and a token endpoint that
	// ... rotates both tokens
	url, _, header := serve(t, toolUseSSE)
	tokenBody := tokenServer(t, http.StatusOK, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":28800}`)
	store, tokens := signedIn(t, Creds{Access: "access-1", Refresh: "refresh-1", ExpiresAt: time.Now().Add(time.Minute), Email: "a@b.c"})
	p := New(Config{BaseURL: url, Tokens: tokens, Model: "m", MaxTokens: 100, Thinking: "adaptive"})
	req := core.ModelRequest{Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}}

	// when
	// ... the request is streamed
	_, err := p.Stream(context.Background(), req, func(string) {})

	// then
	// ... the old refresh token was spent, the new access token sent, and the
	// ... rotated grant saved with the email kept
	if err != nil {
		t.Fatal(err)
	}
	if got := *tokenBody; got["grant_type"] != "refresh_token" || got["refresh_token"] != "refresh-1" {
		t.Errorf("token request = %+v", got)
	}
	if got := header.Get("Authorization"); got != "Bearer access-2" {
		t.Errorf("Authorization = %q, want Bearer access-2", got)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Access != "access-2" || saved.Refresh != "refresh-2" || saved.Email != "a@b.c" {
		t.Errorf("saved = %+v", saved)
	}
}

func TestADeadGrantAsksTheUserToSignInAgain(t *testing.T) {
	// given
	// ... a stale token whose grant the token endpoint no longer honours
	url, _, _ := serve(t, toolUseSSE)
	tokenServer(t, http.StatusBadRequest, `{"error":"invalid_grant"}`)
	_, tokens := signedIn(t, Creds{Access: "access-1", Refresh: "refresh-1", ExpiresAt: time.Now()})
	p := New(Config{BaseURL: url, Tokens: tokens, Model: "m", MaxTokens: 100, Thinking: "adaptive"})
	req := core.ModelRequest{Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}}

	// when
	// ... the request is streamed
	_, err := p.Stream(context.Background(), req, func(string) {})

	// then
	// ... it fails as not signed in, telling the user to log in again
	if !errors.Is(err, ErrNotSignedIn) || !strings.Contains(err.Error(), "grimalkin login") {
		t.Errorf("err = %v, want ErrNotSignedIn naming grimalkin login", err)
	}
}

func TestNoSavedGrantIsNotSignedIn(t *testing.T) {
	// given
	// ... a store whose file was never written
	store := NewStore(filepath.Join(t.TempDir(), "creds.json"))

	// when
	// ... a token source is built from it
	_, err := NewTokenSource(store)

	// then
	// ... it reports not signed in
	if !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("err = %v, want ErrNotSignedIn", err)
	}
}

func TestAGrantIsSavedReadableOnlyByItsOwner(t *testing.T) {
	// given
	// ... a store in a directory that doesn't exist yet
	store := NewStore(filepath.Join(t.TempDir(), "nested", "creds.json"))

	// when
	// ... a grant is saved
	err := store.Save(Creds{Access: "a", Refresh: "r", ExpiresAt: time.Now()})

	// then
	// ... the file is mode 0600
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// browser plays the user approving the sign-in: it calls back with code and
// the state given, or the authorize URL's own state when state is empty.
func browser(t *testing.T, code, state string) (open func(string), authorize *url.URL) {
	t.Helper()
	authorize = new(url.URL)
	return func(raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("authorize URL: %v", err)
		}
		*authorize = *u
		q := u.Query()
		if state == "" {
			state = q.Get("state")
		}
		callback := q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {state}}.Encode()
		resp, err := http.Get(callback)
		if err != nil {
			t.Errorf("callback: %v", err)
			return
		}
		resp.Body.Close()
	}, authorize
}

func TestLoginExchangesTheApprovedCodeForAGrant(t *testing.T) {
	// given
	// ... a browser that approves the sign-in, and a token endpoint issuing a grant
	tokenBody := tokenServer(t, http.StatusOK, `{"access_token":"access-1","refresh_token":"refresh-1","expires_in":28800,"account":{"email_address":"a@b.c"}}`)
	open, authorize := browser(t, "code-1", "")

	// when
	// ... the user logs in
	creds, err := Login(context.Background(), open, io.Discard)

	// then
	// ... the code was exchanged with a verifier matching the PKCE challenge,
	// ... and the grant came back with the account's email
	if err != nil {
		t.Fatal(err)
	}
	got := *tokenBody
	sum := sha256.Sum256([]byte(got["code_verifier"]))
	if challenge := authorize.Query().Get("code_challenge"); base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		t.Errorf("verifier %q does not match challenge %q", got["code_verifier"], challenge)
	}
	if got["grant_type"] != "authorization_code" || got["code"] != "code-1" {
		t.Errorf("token request = %+v", got)
	}
	if creds.Access != "access-1" || creds.Refresh != "refresh-1" || creds.Email != "a@b.c" {
		t.Errorf("creds = %+v", creds)
	}
}

func TestLoginRejectsACallbackWithTheWrongState(t *testing.T) {
	// given
	// ... a callback carrying a state this login never issued
	tokenBody := tokenServer(t, http.StatusOK, `{"access_token":"access-1","refresh_token":"refresh-1","expires_in":28800}`)
	open, _ := browser(t, "code-1", "forged")

	// when
	// ... the user logs in
	_, err := Login(context.Background(), open, io.Discard)

	// then
	// ... the login fails without exchanging the code
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("err = %v, want state mismatch", err)
	}
	if len(*tokenBody) != 0 {
		t.Errorf("token request = %+v, want none", *tokenBody)
	}
}
