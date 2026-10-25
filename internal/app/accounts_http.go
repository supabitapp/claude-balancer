package app

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"net/http"
	"sync"
	"time"
)

const accountLoginTimeout = 15 * time.Minute

type accountLogin struct {
	expiresAt       time.Time
	claudeFlow      *claudeOAuthFlow
	verificationURL string
}

type accountLoginStore struct {
	mu        sync.Mutex
	active    *accountLogin
	completed bool
}

func (s *accountLoginStore) getOrCreateClaude(create func() (*claudeOAuthFlow, string, error)) (accountLogin, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		return *s.active, false, nil
	}
	flow, link, err := create()
	if err != nil {
		return accountLogin{}, false, err
	}
	login := accountLogin{claudeFlow: flow, verificationURL: link, expiresAt: time.Now().Add(accountLoginTimeout)}
	s.active = &login
	s.completed = false
	return login, true, nil
}

func (s *accountLoginStore) finishClaude(flow *claudeOAuthFlow, completed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.claudeFlow == flow {
		s.active = nil
		s.completed = completed
	}
}

func (s *accountLoginStore) status() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active != nil, s.completed
}

func (s *accountLoginStore) submitClaudeCallback(result claudeOAuthCallback) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.active.claudeFlow == nil {
		return errors.New("no Claude sign-in is waiting for a callback")
	}
	if !s.active.expiresAt.IsZero() && time.Now().After(s.active.expiresAt) {
		return errors.New("Claude sign-in has expired; start again")
	}
	return s.active.claudeFlow.submit(result)
}

type accountLoginPageData struct {
	VerificationURL string
	UserCode        string
}

var accountLoginPage = template.Must(webTemplate("accounts.html").ParseFS(dashboardFiles, "web/accounts.html"))

func (s *server) accountsPage(w http.ResponseWriter, r *http.Request) {
	login, created, err := s.logins.getOrCreateClaude(func() (*claudeOAuthFlow, string, error) {
		return startClaudeLogin(s.client, claudeOAuthPort)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var page bytes.Buffer
	if err := accountLoginPage.Execute(&page, accountLoginPageData{
		VerificationURL: login.verificationURL,
		UserCode:        "",
	}); err != nil {
		if created {
			login.claudeFlow.stop()
			s.logins.finishClaude(login.claudeFlow, false)
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if created {
		ctx := s.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		go s.completeClaudeAccountLogin(ctx, login.claudeFlow)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self' "+waterCSSURL+"; script-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(page.Bytes())
}

func (s *server) completeClaudeAccountLogin(ctx context.Context, flow *claudeOAuthFlow) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, accountLoginTimeout)
	defer cancel()
	credentials, err := flow.wait(ctx)
	flow.stop()
	if credentials == nil && err == nil {
		err = errors.New("Claude sign-in returned no credentials")
	}
	var account *Account
	if err == nil {
		account = accountFromClaudeCredentials(credentials)
		err = s.pool.add(account)
	}
	if err != nil {
		s.logins.finishClaude(flow, false)
		if s.stats != nil {
			s.stats.note("account login failed", "", err.Error())
		}
		return
	}
	s.logins.finishClaude(flow, true)
	if s.stats != nil {
		s.stats.note("account added", account.id(), "")
	}
}

func (s *server) accountLoginStatus(w http.ResponseWriter, _ *http.Request) {
	active, completed := s.logins.status()
	if completed {
		w.WriteHeader(http.StatusOK)
		return
	}
	if active {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "account sign-in failed", http.StatusGone)
}

func (s *server) accountLoginCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	var result claudeOAuthCallback
	var err error
	switch r.Method {
	case http.MethodGet:
		result, err = parseClaudeOAuthCallback("http://localhost" + r.URL.RequestURI())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	case http.MethodPost:
		if err = func() error {
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			if err := r.ParseForm(); err != nil {
				return errors.New("invalid callback form")
			}
			result, err = parseClaudeOAuthCallback(r.PostForm.Get("callback_url"))
			return err
		}(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if result.state == "" {
		err = errors.New("callback is missing state")
	} else {
		err = s.logins.submitClaudeCallback(result)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("Claude sign-in received. You can return to the balancer."))
		return
	}
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

func minutesUntil(now, deadline time.Time) int {
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int((remaining + time.Minute - 1) / time.Minute)
}
