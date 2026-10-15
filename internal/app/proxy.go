package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	refreshTimeout = 30 * time.Second
	upstreamWait   = 90 * time.Second
)

func newProxyClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = upstreamWait
	return &http.Client{Transport: transport}
}

type server struct {
	admin            adminAuth
	settingsMu       sync.Mutex
	fastMode         fastModePolicy
	ctx              context.Context
	pool             *Pool
	prices           *priceCatalog
	stats            *Stats
	logins           accountLoginStore
	upstream         string
	lookupAPIKey     func(string) (string, bool, error)
	client           *http.Client
	log              *slog.Logger
	tracer           trace.Tracer
	admission        *admissionGate
	upgradeWait      time.Duration
	resources        *resourceMonitor
	countries        countryResolver
	dashboardStreams atomic.Int64
	dashboardUpdates dashboardBroadcaster
	refreshes        refreshOperations
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	admin := s.adminRoutes()
	mux.Handle("/admin", admin)
	mux.Handle("/admin/", admin)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /accounts", s.accountsPage)
	mux.HandleFunc("GET /accounts/status", s.accountLoginStatus)
	mux.HandleFunc("GET /callback", s.accountLoginCallback)
	mux.HandleFunc("POST /accounts/callback", s.accountLoginCallback)
	mux.HandleFunc(
		"GET /dashboard/assets/admin.css",
		webAsset("web/admin.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable"),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/admin.js",
		webAsset("web/admin.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/dashboard.css",
		webAsset("web/dashboard.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable"),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/accounts.css",
		webAsset(
			"web/accounts.css",
			"text/css; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/accounts.js",
		webAsset(
			"web/accounts.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc("GET /dashboard", s.dashboardPage)
	mux.HandleFunc(
		"GET /favicon.svg",
		webAsset("web/favicon.svg", "image/svg+xml", "public, max-age=3600"),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/dashboard.js",
		webAsset(
			"web/dashboard.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/htmx-2.0.10.min.js",
		webAsset(
			"web/htmx-2.0.10.min.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/idiomorph-0.7.4.min.js",
		webAsset(
			"web/idiomorph-0.7.4.min.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc(
		"GET /dashboard/assets/sse-2.2.4.min.js",
		webAsset(
			"web/sse-2.2.4.min.js",
			"text/javascript; charset=utf-8",
			"public, max-age=31536000, immutable",
		),
	)
	mux.HandleFunc("GET /dashboard/events", s.dashboardEvents)
	mux.HandleFunc("GET /stats", s.statsJSON)
	claudeMessages := s.admitted(s.claudeMessagesHTTP)
	for _, path := range []string{"/v1/messages", "/messages", "/v1/messages/count_tokens", "/messages/count_tokens"} {
		mux.Handle("POST "+path, claudeMessages)
		mux.Handle(path, http.HandlerFunc(s.claudeMessagesMethodNotAllowed))
	}
	mux.HandleFunc("GET /v1/models", s.claudeModels)
	return mux
}

func (s *server) refreshed(account *Account, id string) bool {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return s.refreshedContext(ctx, account, id)
}

func (s *server) refreshedContext(ctx context.Context, account *Account, id string) bool {
	ctx, waitSpan := s.responseTracer().Start(ctx, "claude.credentials.wait")
	defer waitSpan.End()
	started := time.Now()
	observation(ctx).event(ctx, "refresh_wait_started", attribute.String("account", id))
	s.log.Debug("refreshing account", "account", id)
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	lifetime := s.ctx
	if lifetime == nil {
		lifetime = context.Background()
	}
	owner := refreshOwner{
		lifetime: lifetime, client: s.client, operations: &s.refreshes, tracer: s.responseTracer(), log: s.log,
		persist: s.pool.persistAccountStateContext,
		completed: func(_ context.Context, err error) error {
			if err != nil {
				s.log.Warn("refresh failed", "account", id, "error", err)
			} else {
				s.log.Debug("account refreshed", "account", id)
			}
			return nil
		},
	}
	if err := account.refreshOwned(ctx, owner); err != nil {
		spanFailure(waitSpan, err)
		observation(ctx).event(ctx, "refresh_wait_finished", attribute.String("account", id), attribute.Bool("success", false), attribute.String("error_type", telemetryErrorClass(err)), attribute.Int64("elapsed_ms", time.Since(started).Milliseconds()))
		s.log.Debug("refresh wait ended", "account", id, "error", err)
		return false
	}
	observation(ctx).event(ctx, "refresh_wait_finished", attribute.String("account", id), attribute.Bool("success", true), attribute.Int64("elapsed_ms", time.Since(started).Milliseconds()))
	return true
}

type apiKeyIdentity struct {
	name   string
	suffix string
}

func (s *server) authorizeAPIKey(r *http.Request) (apiKeyIdentity, bool) {
	if s.lookupAPIKey == nil {
		return apiKeyIdentity{}, true
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	name, valid, err := s.lookupAPIKey(presented)
	if err != nil {
		if s.log != nil {
			s.log.Error("API key lookup failed", "error", err)
		}
		return apiKeyIdentity{}, false
	}
	if !valid {
		return apiKeyIdentity{}, false
	}
	return apiKeyIdentity{name: name, suffix: tokenSuffix(presented)}, true
}

func tokenSuffix(token string) string {
	if len(token) <= 3 {
		return token
	}
	return token[len(token)-3:]
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": "balancer_error"},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
