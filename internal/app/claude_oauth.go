package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthScope    = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	claudeOAuthPort     = 54545
	claudeOAuthPath     = "/callback"
)

var (
	claudeOAuthAuthorizeURL = "https://claude.ai/oauth/authorize"
	claudeOAuthTokenURL     = "https://platform.claude.com/v1/oauth/token"
	claudeOAuthProfileURL   = "https://api.anthropic.com/api/oauth/profile"
	claudeOAuthRolesURL     = "https://api.anthropic.com/api/oauth/claude_cli/roles"
)

type claudePKCE struct {
	verifier  string
	challenge string
}

type claudeCredentials struct {
	AccessToken      string
	RefreshToken     string
	AccountID        string
	Email            string
	OrganizationID   string
	OrganizationName string
	ExpiresAt        time.Time
	LastRefresh      time.Time
}

type claudeOAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Organization struct {
		ID   string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
	Account struct {
		ID    string `json:"uuid"`
		Email string `json:"email_address"`
	} `json:"account"`
}

type claudeOAuthProfile struct {
	Account struct {
		ID    string `json:"uuid"`
		Email string `json:"email"`
	} `json:"account"`
	Organization struct {
		ID   string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

type claudeOAuthCallback struct {
	code  string
	state string
	err   string
}

type claudeOAuthFlow struct {
	state    string
	pkce     claudePKCE
	callback chan claudeOAuthCallback
	once     sync.Once
	listener net.Listener
	server   *http.Server
	redirect string
	hc       *http.Client
}

func newClaudePKCE() (claudePKCE, error) {
	verifierBytes := make([]byte, 96)
	if _, err := rand.Read(verifierBytes); err != nil {
		return claudePKCE{}, fmt.Errorf("generate Claude PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	digest := sha256.Sum256([]byte(verifier))
	return claudePKCE{verifier: verifier, challenge: base64.RawURLEncoding.EncodeToString(digest[:])}, nil
}

func claudeOAuthRedirect(port int) string {
	if port <= 0 {
		port = claudeOAuthPort
	}
	return "http://localhost:" + strconv.Itoa(port) + claudeOAuthPath
}

func claudeAuthorizationURL(state string, pkce claudePKCE, redirect string) string {
	query := url.Values{
		"code":                  {"true"},
		"client_id":             {claudeOAuthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"scope":                 {claudeOAuthScope},
		"code_challenge":        {pkce.challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return claudeOAuthAuthorizeURL + "?" + query.Encode()
}

func claudeOAuthHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "axios/1.15.2")
	req.Header.Set("Cache-Control", "no-cache")
	req.Close = true
}

func claudeLogin(ctx context.Context, hc *http.Client, port int, open bool) (*claudeCredentials, error) {
	flow, link, err := startClaudeLogin(hc, port)
	if err != nil {
		return nil, err
	}
	defer flow.stop()
	if open {
		openBrowser(link)
	}
	return flow.wait(ctx)
}

func startClaudeLogin(hc *http.Client, port int) (*claudeOAuthFlow, string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if port <= 0 {
		port = claudeOAuthPort
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return nil, "", fmt.Errorf("Claude OAuth callback address is unavailable: %w", err)
	}
	pkce, err := newClaudePKCE()
	if err != nil {
		listener.Close()
		return nil, "", err
	}
	state, err := randomClaudeOAuthToken(32)
	if err != nil {
		listener.Close()
		return nil, "", fmt.Errorf("generate Claude OAuth state: %w", err)
	}
	flow := &claudeOAuthFlow{
		state: state, pkce: pkce,
		callback: make(chan claudeOAuthCallback, 1), listener: listener,
		redirect: claudeOAuthRedirect(port), hc: hc,
	}
	flow.server = &http.Server{Handler: flow.handler()}
	go func() { _ = flow.server.Serve(listener) }()
	return flow, claudeAuthorizationURL(state, pkce, flow.redirect), nil
}

func (f *claudeOAuthFlow) wait(ctx context.Context) (*claudeCredentials, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case result := <-f.callback:
		if result.err != "" {
			return nil, fmt.Errorf("Claude sign-in refused: %s", result.err)
		}
		if result.state != f.state {
			return nil, errors.New("Claude OAuth state mismatch")
		}
		return exchangeClaudeCode(ctx, f.hc, result.code, f.state, f.pkce, f.redirect)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *claudeOAuthFlow) stop() {
	if f == nil || f.server == nil {
		return
	}
	_ = f.server.Shutdown(context.Background())
}

func (f *claudeOAuthFlow) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(claudeOAuthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query := r.URL.Query()
		result := claudeOAuthCallback{code: query.Get("code"), state: query.Get("state"), err: query.Get("error")}
		if result.err == "" {
			result.err = query.Get("error_description")
		}
		f.once.Do(func() { f.callback <- result })
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if result.err != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Claude sign-in failed: "+html.EscapeString(result.err))
			return
		}
		_, _ = io.WriteString(w, "Claude sign-in complete. You can close this window.")
	})
	return mux
}

func exchangeClaudeCode(ctx context.Context, hc *http.Client, code, state string, pkce claudePKCE, redirect string) (*claudeCredentials, error) {
	if strings.Contains(code, "#") {
		parts := strings.SplitN(code, "#", 2)
		fragmentState := parts[1]
		code = parts[0]
		if fragmentState != "" {
			state = fragmentState
		}
	}
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("Claude OAuth callback carried no authorization code")
	}
	body := struct {
		GrantType    string `json:"grant_type"`
		Code         string `json:"code"`
		RedirectURI  string `json:"redirect_uri"`
		ClientID     string `json:"client_id"`
		CodeVerifier string `json:"code_verifier"`
		State        string `json:"state"`
	}{"authorization_code", code, redirect, claudeOAuthClientID, pkce.verifier, state}
	var response claudeOAuthTokenResponse
	if err := postClaudeOAuthJSON(ctx, hc, claudeOAuthTokenURL, body, &response); err != nil {
		return nil, fmt.Errorf("Claude token exchange failed: %w", err)
	}
	credentials := claudeCredentialsFromToken(response, time.Now())
	if credentials.AccessToken == "" {
		return nil, errors.New("Claude token exchange returned no access token")
	}
	if profile, err := fetchClaudeOAuthProfile(ctx, hc, credentials.AccessToken); err == nil {
		mergeClaudeProfile(&credentials, profile)
	}
	if credentials.AccountID == "" {
		return nil, errors.New("Claude token exchange returned no account identity")
	}
	return &credentials, nil
}

func refreshClaudeToken(ctx context.Context, hc *http.Client, refreshToken string) (*claudeCredentials, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, errors.New("Claude refresh token is required")
	}
	body := struct {
		ClientID     string `json:"client_id"`
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}{claudeOAuthClientID, "refresh_token", refreshToken, claudeOAuthScope}
	var response claudeOAuthTokenResponse
	if err := postClaudeOAuthJSON(ctx, hc, claudeOAuthTokenURL, body, &response); err != nil {
		return nil, err
	}
	credentials := claudeCredentialsFromToken(response, time.Now())
	if credentials.RefreshToken == "" {
		credentials.RefreshToken = refreshToken
	}
	if credentials.AccessToken == "" {
		return nil, errors.New("Claude refresh returned no access token")
	}
	if profile, err := fetchClaudeOAuthProfile(ctx, hc, credentials.AccessToken); err == nil {
		mergeClaudeProfile(&credentials, profile)
	}
	return &credentials, nil
}

func postClaudeOAuthJSON(ctx context.Context, hc *http.Client, endpoint string, body any, into any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(encoded)))
	if err != nil {
		return err
	}
	claudeOAuthHeaders(req)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return fmt.Errorf("decode Claude OAuth response: %w", err)
	}
	return nil
}

func fetchClaudeOAuthProfile(ctx context.Context, hc *http.Client, accessToken string) (*claudeOAuthProfile, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("Claude access token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeOAuthProfileURL, nil)
	if err != nil {
		return nil, err
	}
	claudeOAuthHeaders(req)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("Claude profile endpoint returned %s", resp.Status)
	}
	var profile claudeOAuthProfile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return nil, err
	}
	if profile.Account.ID == "" {
		return nil, errors.New("Claude profile response omitted account UUID")
	}
	return &profile, nil
}

func claudeCredentialsFromToken(token claudeOAuthTokenResponse, now time.Time) claudeCredentials {
	credentials := claudeCredentials{
		AccessToken:      token.AccessToken,
		RefreshToken:     token.RefreshToken,
		AccountID:        token.Account.ID,
		Email:            token.Account.Email,
		OrganizationID:   token.Organization.ID,
		OrganizationName: token.Organization.Name,
		LastRefresh:      now,
	}
	if token.ExpiresIn > 0 {
		credentials.ExpiresAt = now.Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	return credentials
}

func accountFromClaudeCredentials(credentials *claudeCredentials) *Account {
	if credentials == nil {
		return nil
	}
	return accountFromState(accountState{
		IDToken:      claudeIdentityToken(credentials.AccountID, credentials.Email),
		AccessToken:  credentials.AccessToken,
		RefreshToken: credentials.RefreshToken,
		LastRefresh:  credentials.LastRefresh,
	})
}

func claudeIdentityToken(accountID, email string) string {
	payload, _ := json.Marshal(struct {
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
			Plan      string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}{Email: email, Auth: struct {
		AccountID string `json:"chatgpt_account_id"`
		Plan      string `json:"chatgpt_plan_type"`
	}{AccountID: accountID, Plan: "claude"}})
	return "x." + base64.RawURLEncoding.EncodeToString(payload) + ".x"
}

func mergeClaudeProfile(credentials *claudeCredentials, profile *claudeOAuthProfile) {
	if credentials == nil || profile == nil {
		return
	}
	if profile.Account.ID != "" {
		credentials.AccountID = profile.Account.ID
	}
	if profile.Account.Email != "" {
		credentials.Email = profile.Account.Email
	}
	if profile.Organization.ID != "" {
		credentials.OrganizationID = profile.Organization.ID
	}
	if profile.Organization.Name != "" {
		credentials.OrganizationName = profile.Organization.Name
	}
}

func randomClaudeOAuthToken(size int) (string, error) {
	if size < 1 {
		return "", errors.New("token size must be positive")
	}
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
