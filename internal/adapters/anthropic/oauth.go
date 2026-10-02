package anthropic

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// The public OAuth client Claude Code registers for Pro/Max sign-in. The
// authorize endpoint must be claude.ai: platform.claude.com issues console
// tokens, which cannot call inference.
const (
	clientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	authorizeURL = "https://claude.ai/oauth/authorize"
	callbackAddr = "127.0.0.1:54545"
	redirectURI  = "http://localhost:54545/callback"

	// refreshSkew pulls the refresh forward so a token cannot expire between
	// the check and the response arriving.
	refreshSkew = 5 * time.Minute
	loginWait   = 5 * time.Minute
)

// tokenURL is a variable only so tests can point it at a local server.
var tokenURL = "https://api.anthropic.com/v1/oauth/token"

// user:inference makes the token usable against /v1/messages; the rest match
// what Claude Code itself asks for.
var scopes = []string{
	"org:create_api_key",
	"user:profile",
	"user:inference",
	"user:sessions:claude_code",
	"user:mcp_servers",
	"user:file_upload",
}

// Claude Code's wire fingerprint. A Pro/Max token is only accepted for
// inference when the request looks like it came from the CLI, so these are
// load bearing: the identity system block in particular is checked.
const (
	claudeCodeVersion           = "2.1.287"
	claudeCodeSDKVersion        = "0.127.0"
	claudeCodeSystemInstruction = "You are Claude Code, Anthropic's official CLI for Claude."
	oauthBeta                   = "oauth-2025-04-20"
)

var oauthBetas = []string{
	"claude-code-20250219",
	oauthBeta,
	"interleaved-thinking-2025-05-14",
	"effort-2025-11-24",
}

var ErrNotSignedIn = errors.New("not signed in")

// Creds is one OAuth grant. Refresh rotates the refresh token, so a successful
// refresh must be saved or the next one fails.
type Creds struct {
	Access    string    `json:"access"`
	Refresh   string    `json:"refresh"`
	ExpiresAt time.Time `json:"expires_at"`
	Email     string    `json:"email,omitempty"`
}

func (c Creds) stale(now time.Time) bool {
	return !now.Add(refreshSkew).Before(c.ExpiresAt)
}

// Store is the credential file. It holds a live subscription token, so it is
// written 0600 and never logged.
type Store struct{ path string }

func NewStore(path string) Store { return Store{path: path} }

func (s Store) Path() string { return s.path }

func (s Store) Load() (Creds, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Creds{}, ErrNotSignedIn
	}
	if err != nil {
		return Creds{}, err
	}
	var c Creds
	if err := json.Unmarshal(data, &c); err != nil {
		return Creds{}, fmt.Errorf("%s is corrupt, delete it and sign in again: %w", s.path, err)
	}
	if c.Access == "" || c.Refresh == "" {
		return Creds{}, ErrNotSignedIn
	}
	return c, nil
}

func (s Store) Save(c Creds) error {
	if c.Access == "" || c.Refresh == "" {
		panic("refusing to save an incomplete grant")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

func randomToken() string {
	raw := make([]byte, 32)
	rand.Read(raw) // never returns an error; crashes the program on failure
	return base64.RawURLEncoding.EncodeToString(raw)
}

// tokenResponse is the shared shape of the exchange and refresh replies.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Account      struct {
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}

func (t tokenResponse) creds(now time.Time, prev Creds) (Creds, error) {
	if t.AccessToken == "" {
		return Creds{}, errors.New("token endpoint returned no access_token")
	}
	if t.ExpiresIn <= 0 {
		return Creds{}, fmt.Errorf("token endpoint returned expires_in=%d", t.ExpiresIn)
	}
	c := Creds{
		Access:    t.AccessToken,
		Refresh:   t.RefreshToken,
		ExpiresAt: now.Add(time.Duration(t.ExpiresIn) * time.Second),
		Email:     t.Account.EmailAddress,
	}
	// The refresh reply may omit what the login reply carried.
	if c.Refresh == "" {
		c.Refresh = prev.Refresh
	}
	if c.Email == "" {
		c.Email = prev.Email
	}
	if c.Refresh == "" {
		return Creds{}, errors.New("token endpoint returned no refresh_token and none was held")
	}
	return c, nil
}

func postToken(ctx context.Context, body map[string]string, headers map[string]string) (tokenResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return tokenResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(payload))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return tokenResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out tokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return tokenResponse{}, fmt.Errorf("token endpoint returned invalid JSON: %w", err)
	}
	return out, nil
}

// Login runs the browser sign-in and returns a fresh grant. open is handed the
// authorize URL, which is also written to out in case it fails. The listener
// binds before the browser opens so a busy port fails before the user has
// approved anything.
func Login(ctx context.Context, open func(url string), out io.Writer) (Creds, error) {
	verifier := randomToken()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomToken()

	listener, err := net.Listen("tcp", callbackAddr)
	if err != nil {
		return Creds{}, fmt.Errorf("cannot listen on %s for the sign-in callback: %w", callbackAddr, err)
	}
	type callback struct {
		code string
		err  error
	}
	results := make(chan callback, 1)
	report := func(cb callback) {
		select {
		case results <- cb:
		default: // only the first callback counts
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "sign-in denied", http.StatusBadRequest)
			report(callback{err: fmt.Errorf("sign-in denied: %s %s", e, q.Get("error_description"))})
			return
		}
		// The callback is reachable by anything on loopback; state is the only
		// thing binding it to this login.
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			report(callback{err: errors.New("state mismatch on the sign-in callback")})
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			report(callback{err: errors.New("sign-in callback carried no code")})
			return
		}
		io.WriteString(w, "Signed in to grimalkin. You can close this tab.")
		report(callback{code: code})
	})
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	params := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(scopes, " ")},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"code":                  {"true"},
	}
	authURL := authorizeURL + "?" + params.Encode()
	fmt.Fprintf(out, "Opening your browser to sign in with your Claude account.\nIf it does not open, visit:\n  %s\n", authURL)
	open(authURL)

	var cb callback
	select {
	case cb = <-results:
	case <-ctx.Done():
		return Creds{}, ctx.Err()
	case <-time.After(loginWait):
		return Creds{}, errors.New("timed out waiting for the browser to call back")
	}
	if cb.err != nil {
		return Creds{}, cb.err
	}

	// The code may arrive as code#state; the fragment's state is the one to send.
	code, exchangeState := cb.code, state
	if i := strings.Index(code, "#"); i >= 0 {
		code, exchangeState = code[:i], code[i+1:]
	}
	resp, err := postToken(ctx, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     clientID,
		"code":          code,
		"state":         exchangeState,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	}, nil)
	if err != nil {
		return Creds{}, err
	}
	return resp.creds(time.Now(), Creds{})
}

func refreshGrant(ctx context.Context, prev Creds) (Creds, error) {
	resp, err := postToken(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     clientID,
		"refresh_token": prev.Refresh,
	}, map[string]string{
		"anthropic-beta": oauthBeta,
		"User-Agent":     "anthropic-sdk-typescript/" + claudeCodeSDKVersion + " userOAuthProvider",
	})
	if err != nil {
		// The grant dies roughly 30 days after the browser sign-in however
		// healthy rotation has been. Only a new login recovers it.
		if strings.Contains(err.Error(), "invalid_grant") {
			return Creds{}, fmt.Errorf("%w: run `grimalkin login` again", ErrNotSignedIn)
		}
		return Creds{}, fmt.Errorf("refresh sign-in: %w", err)
	}
	return resp.creds(time.Now(), prev)
}

// TokenSource hands out a valid access token, refreshing and saving the
// rotated grant when the held one is close to expiry.
type TokenSource struct {
	store Store
	mu    sync.Mutex
	creds Creds
}

func NewTokenSource(s Store) (*TokenSource, error) {
	c, err := s.Load()
	if err != nil {
		return nil, err
	}
	return &TokenSource{store: s, creds: c}, nil
}

func (t *TokenSource) token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.creds.stale(time.Now()) {
		return t.creds.Access, nil
	}
	next, err := refreshGrant(ctx, t.creds)
	if err != nil {
		return "", err
	}
	if err := t.store.Save(next); err != nil {
		return "", fmt.Errorf("refreshed sign-in but could not save the rotated grant: %w", err)
	}
	t.creds = next
	return t.creds.Access, nil
}

// oauthHeaders are the CLI fingerprint sent on every OAuth request.
func oauthHeaders() []option.RequestOption {
	headers := map[string]string{
		"User-Agent":     "claude-cli/" + claudeCodeVersion + " (external, cli)",
		"Anthropic-Beta": strings.Join(oauthBetas, ","),
		"X-App":          "cli",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"X-Stainless-Arch":                          stainlessArch(),
		"X-Stainless-Lang":                          "js",
		"X-Stainless-Os":                            stainlessOS(),
		"X-Stainless-Package-Version":               claudeCodeSDKVersion,
		"X-Stainless-Runtime":                       "node",
		"X-Stainless-Runtime-Version":               "v26.3.0",
		"X-Stainless-Timeout":                       "600",
	}
	opts := make([]option.RequestOption, 0, len(headers))
	for k, v := range headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return opts
}

func stainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "linux":
		return "Linux"
	default:
		return "Other::" + runtime.GOOS
	}
}

func stainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	default:
		return "other::" + runtime.GOARCH
	}
}
