package controlplane

import (
	"context"
	"crypto/hmac"
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
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	defaultSessionCookie = "gateway_session"
	defaultStateCookie   = "gateway_oauth_state"
	defaultPKCECookie    = "gateway_oauth_pkce"
	defaultCSRFCookie    = "gateway_csrf"
	defaultSessionTTL    = 7 * 24 * time.Hour
	defaultStateTTL      = 10 * time.Minute
	defaultLoginRedirect = "/"
	defaultOAuthTimeout  = 10 * time.Second

	githubAuthURL  = "https://github.com/login/oauth/authorize"
	githubTokenURL = "https://github.com/login/oauth/access_token"
	githubUserURL  = "https://api.github.com/user"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrNotFound        = errors.New("not found")
)

type User struct {
	ID          string `json:"id"`
	GitHubID    int64  `json:"github_id,omitempty"`
	GitHubLogin string `json:"github_login"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

type GitHubUser struct {
	GitHubID    int64
	GitHubLogin string
	AvatarURL   string
}

type Session struct {
	User      User
	ExpiresAt time.Time
}

type NewSession struct {
	UserID    string
	TokenHash []byte
	ExpiresAt time.Time
	Now       time.Time
}

type UserStore interface {
	UpsertGitHubUser(context.Context, GitHubUser) (User, error)
}

type SessionStore interface {
	CreateSession(context.Context, NewSession) error
	GetSession(context.Context, []byte, time.Time) (Session, error)
	DeleteSession(context.Context, []byte) error
}

type AuthConfig struct {
	OAuth              oauth2.Config
	GitHubUserURL      string
	PublicConsoleURL   string
	LoginRedirectPath  string
	SessionCookieName  string
	StateCookieName    string
	PKCECookieName     string
	CSRFCookieName     string
	SessionTokenPepper []byte
	SessionTTL         time.Duration
	StateTTL           time.Duration
	SecureCookies      bool
	HTTPClient         *http.Client
	Now                func() time.Time
	Random             io.Reader
}

type AuthHandler struct {
	oauth             oauth2.Config
	githubUserURL     string
	publicOrigin      origin
	loginRedirectPath string
	sessionCookieName string
	stateCookieName   string
	pkceCookieName    string
	csrfCookieName    string
	sessionPepper     []byte
	sessionTTL        time.Duration
	stateTTL          time.Duration
	secureCookies     bool
	client            *http.Client
	now               func() time.Time
	random            io.Reader
	users             UserStore
	sessions          SessionStore
}

type contextUserKey struct{}

type origin struct {
	scheme string
	host   string
	port   string
}

func NewAuthHandler(config AuthConfig, users UserStore, sessions SessionStore) (*AuthHandler, error) {
	if users == nil {
		return nil, errors.New("user store is required")
	}
	if sessions == nil {
		return nil, errors.New("session store is required")
	}
	if strings.TrimSpace(config.OAuth.ClientID) == "" {
		return nil, errors.New("github OAuth client ID is required")
	}
	if strings.TrimSpace(config.OAuth.ClientSecret) == "" {
		return nil, errors.New("github OAuth client secret is required")
	}
	if strings.TrimSpace(config.OAuth.RedirectURL) == "" {
		return nil, errors.New("github OAuth redirect URL is required")
	}
	if len(config.SessionTokenPepper) < 32 {
		return nil, errors.New("session token pepper must be at least 32 bytes")
	}
	if err := rejectOAuthScopes(config.OAuth.Scopes); err != nil {
		return nil, err
	}

	publicOrigin, err := canonicalOrigin(config.PublicConsoleURL, true)
	if err != nil {
		return nil, errors.New("public console URL must be an origin URL")
	}
	if publicOrigin.scheme == "https" {
		if !config.SecureCookies {
			return nil, errors.New("https public console requires secure cookies")
		}
	} else if !isLoopback(publicOrigin.host) {
		return nil, errors.New("public console URL must use https except for loopback development")
	}
	parsedPublicURL, _ := url.Parse(config.PublicConsoleURL)
	loginRedirectPath := valueOrDefault(config.LoginRedirectPath, defaultLoginRedirect)
	if !strings.HasPrefix(loginRedirectPath, "/") || strings.HasPrefix(loginRedirectPath, "//") {
		return nil, errors.New("login redirect path must be a same-origin absolute path")
	}

	oauthConfig := config.OAuth
	if oauthConfig.Endpoint.AuthURL == "" {
		oauthConfig.Endpoint.AuthURL = githubAuthURL
	}
	if oauthConfig.Endpoint.TokenURL == "" {
		oauthConfig.Endpoint.TokenURL = githubTokenURL
	}
	wantRedirectURL := strings.TrimRight(parsedPublicURL.String(), "/") + "/auth/github/callback"
	if oauthConfig.RedirectURL != wantRedirectURL {
		return nil, errors.New("OAuth redirect URL must match the public console callback URL")
	}

	handler := &AuthHandler{
		oauth:             oauthConfig,
		githubUserURL:     valueOrDefault(config.GitHubUserURL, githubUserURL),
		publicOrigin:      publicOrigin,
		loginRedirectPath: loginRedirectPath,
		sessionCookieName: valueOrDefault(config.SessionCookieName, defaultSessionCookie),
		stateCookieName:   valueOrDefault(config.StateCookieName, defaultStateCookie),
		pkceCookieName:    valueOrDefault(config.PKCECookieName, defaultPKCECookie),
		csrfCookieName:    valueOrDefault(config.CSRFCookieName, defaultCSRFCookie),
		sessionPepper:     append([]byte(nil), config.SessionTokenPepper...),
		sessionTTL:        durationOrDefault(config.SessionTTL, defaultSessionTTL),
		stateTTL:          durationOrDefault(config.StateTTL, defaultStateTTL),
		secureCookies:     config.SecureCookies,
		client:            config.HTTPClient,
		now:               config.Now,
		random:            config.Random,
		users:             users,
		sessions:          sessions,
	}
	if handler.client == nil {
		handler.client = &http.Client{Timeout: defaultOAuthTimeout}
	}
	if handler.now == nil {
		handler.now = time.Now
	}
	if handler.random == nil {
		handler.random = rand.Reader
	}
	return handler, nil
}

func (h *AuthHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/github/login", h.handleGitHubLogin)
	mux.HandleFunc("GET /auth/github/callback", h.handleGitHubCallback)
	mux.Handle("POST /auth/logout", h.RequireSameOrigin(http.HandlerFunc(h.handleLogout)))
	mux.HandleFunc("GET /api/v1/me", h.handleMe)
}

func (h *AuthHandler) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		user, err := h.sessionUser(request.Context(), request)
		if err != nil {
			writeSessionError(response, err)
			return
		}
		next.ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), contextUserKey{}, user)))
	})
}

func (h *AuthHandler) RequireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if isSafeMethod(request.Method) {
			next.ServeHTTP(response, request)
			return
		}
		if !h.hasValidOrigin(request) || !h.hasValidCSRF(request) {
			writeJSONError(response, http.StatusForbidden, "forbidden")
			return
		}
		next.ServeHTTP(response, request)
	})
}

func UserFromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(contextUserKey{}).(User)
	return user, ok
}

func (h *AuthHandler) handleGitHubLogin(response http.ResponseWriter, request *http.Request) {
	state, err := randomToken(h.random, 32)
	if err != nil {
		writeJSONError(response, http.StatusInternalServerError, "internal_error")
		return
	}
	verifier, err := randomToken(h.random, 32)
	if err != nil {
		writeJSONError(response, http.StatusInternalServerError, "internal_error")
		return
	}

	stateExpires := h.now().Add(h.stateTTL)
	http.SetCookie(response, h.transientCookie(h.stateCookieName, state, stateExpires))
	http.SetCookie(response, h.transientCookie(h.pkceCookieName, verifier, stateExpires))
	http.Redirect(response, request, h.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (h *AuthHandler) handleGitHubCallback(response http.ResponseWriter, request *http.Request) {
	// Every callback attempt consumes the transient values, including invalid
	// attempts, so a state/verifier pair cannot be replayed within its TTL.
	h.clearOAuthCookies(response)
	stateCookie, err := request.Cookie(h.stateCookieName)
	queryState := request.URL.Query().Get("state")
	if err != nil || stateCookie.Value == "" || queryState == "" || !constantTimeEqual(stateCookie.Value, queryState) {
		writeJSONError(response, http.StatusBadRequest, "invalid_oauth_state")
		return
	}
	pkceCookie, err := request.Cookie(h.pkceCookieName)
	if err != nil || strings.TrimSpace(pkceCookie.Value) == "" {
		writeJSONError(response, http.StatusBadRequest, "invalid_oauth_state")
		return
	}
	code := request.URL.Query().Get("code")
	if strings.TrimSpace(code) == "" {
		writeJSONError(response, http.StatusBadRequest, "missing_oauth_code")
		return
	}

	ctx := context.WithValue(request.Context(), oauth2.HTTPClient, h.client)
	token, err := h.oauth.Exchange(ctx, code, oauth2.VerifierOption(pkceCookie.Value))
	if err != nil {
		writeJSONError(response, http.StatusBadGateway, "oauth_exchange_failed")
		return
	}

	githubUser, err := h.fetchGitHubUser(request.Context(), token.AccessToken)
	if err != nil {
		writeJSONError(response, http.StatusBadGateway, "github_user_failed")
		return
	}
	user, err := h.users.UpsertGitHubUser(request.Context(), githubUser)
	if err != nil {
		writeJSONError(response, http.StatusInternalServerError, "user_persist_failed")
		return
	}

	sessionToken, err := randomToken(h.random, 32)
	if err != nil {
		writeJSONError(response, http.StatusInternalServerError, "internal_error")
		return
	}
	csrfToken, err := randomToken(h.random, 32)
	if err != nil {
		writeJSONError(response, http.StatusInternalServerError, "internal_error")
		return
	}

	now := h.now()
	if err := h.sessions.CreateSession(request.Context(), NewSession{
		UserID:    user.ID,
		TokenHash: h.sessionTokenHash(sessionToken),
		ExpiresAt: now.Add(h.sessionTTL),
		Now:       now,
	}); err != nil {
		writeJSONError(response, http.StatusInternalServerError, "session_persist_failed")
		return
	}

	http.SetCookie(response, h.sessionCookie(sessionToken, now.Add(h.sessionTTL)))
	http.SetCookie(response, h.csrfCookie(csrfToken, now.Add(h.sessionTTL)))
	http.Redirect(response, request, h.loginRedirectPath, http.StatusSeeOther)
}

func (h *AuthHandler) handleLogout(response http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(h.sessionCookieName); err == nil {
		if err := h.sessions.DeleteSession(request.Context(), h.sessionTokenHash(cookie.Value)); err != nil {
			writeJSONError(response, http.StatusInternalServerError, "session_delete_failed")
			return
		}
	}
	h.clearSessionCookies(response)
	response.WriteHeader(http.StatusNoContent)
}

func (h *AuthHandler) handleMe(response http.ResponseWriter, request *http.Request) {
	user, err := h.sessionUser(request.Context(), request)
	if err != nil {
		writeSessionError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, struct {
		User User `json:"user"`
	}{User: user})
}

func (h *AuthHandler) sessionUser(ctx context.Context, request *http.Request) (User, error) {
	cookie, err := request.Cookie(h.sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return User{}, ErrUnauthenticated
	}
	session, err := h.sessions.GetSession(ctx, h.sessionTokenHash(cookie.Value), h.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return User{}, ErrUnauthenticated
		}
		return User{}, err
	}
	return session.User, nil
}

func (h *AuthHandler) fetchGitHubUser(ctx context.Context, token string) (GitHubUser, error) {
	if strings.TrimSpace(token) == "" {
		return GitHubUser{}, errors.New("empty github access token")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.githubUserURL, nil)
	if err != nil {
		return GitHubUser{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := h.client.Do(request)
	if err != nil {
		return GitHubUser{}, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return GitHubUser{}, fmt.Errorf("github user status %d", response.StatusCode)
	}

	var payload struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return GitHubUser{}, err
	}
	if payload.ID <= 0 || strings.TrimSpace(payload.Login) == "" {
		return GitHubUser{}, errors.New("github user response missing required fields")
	}
	return GitHubUser{
		GitHubID:    payload.ID,
		GitHubLogin: payload.Login,
		AvatarURL:   payload.AvatarURL,
	}, nil
}

func (h *AuthHandler) sessionTokenHash(token string) []byte {
	mac := hmac.New(sha256.New, h.sessionPepper)
	_, _ = mac.Write([]byte(token))
	return mac.Sum(nil)
}

func (h *AuthHandler) transientCookie(name, value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/auth",
		Expires:  expires,
		MaxAge:   cookieMaxAge(expires.Sub(h.now())),
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

func (h *AuthHandler) sessionCookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     h.sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   cookieMaxAge(expires.Sub(h.now())),
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

func (h *AuthHandler) csrfCookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     h.csrfCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   cookieMaxAge(expires.Sub(h.now())),
		HttpOnly: false,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

func (h *AuthHandler) clearOAuthCookies(response http.ResponseWriter) {
	http.SetCookie(response, expiredCookie(h.stateCookieName, "/auth", h.secureCookies, true))
	http.SetCookie(response, expiredCookie(h.pkceCookieName, "/auth", h.secureCookies, true))
}

func (h *AuthHandler) clearSessionCookies(response http.ResponseWriter) {
	http.SetCookie(response, expiredCookie(h.sessionCookieName, "/", h.secureCookies, true))
	http.SetCookie(response, expiredCookie(h.csrfCookieName, "/", h.secureCookies, false))
}

func expiredCookie(name, path string, secure, httpOnly bool) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (h *AuthHandler) hasValidOrigin(request *http.Request) bool {
	requestOrigin, err := canonicalOrigin(request.Header.Get("Origin"), false)
	if err != nil {
		return false
	}
	return requestOrigin == h.publicOrigin
}

func (h *AuthHandler) hasValidCSRF(request *http.Request) bool {
	header := request.Header.Get("X-CSRF-Token")
	cookie, err := request.Cookie(h.csrfCookieName)
	if err != nil || header == "" || cookie.Value == "" {
		return false
	}
	return constantTimeEqual(header, cookie.Value)
}

func rejectOAuthScopes(scopes []string) error {
	for _, scope := range scopes {
		if strings.TrimSpace(scope) != "" {
			return fmt.Errorf("github OAuth scope %q is not allowed; login requests identity only", scope)
		}
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func canonicalOrigin(raw string, allowRootPath bool) (origin, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return origin{}, errors.New("invalid origin")
	}
	if parsed.Path != "" && (!allowRootPath || parsed.Path != "/") {
		return origin{}, errors.New("origin must not contain a path")
	}

	scheme := strings.ToLower(parsed.Scheme)
	defaultPort := ""
	switch scheme {
	case "https":
		defaultPort = "443"
	case "http":
		defaultPort = "80"
	default:
		return origin{}, errors.New("origin scheme must be http or https")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return origin{}, errors.New("origin host is required")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}

	port := parsed.Port()
	if port == "" {
		port = defaultPort
	} else {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return origin{}, errors.New("invalid origin port")
		}
		port = strconv.Itoa(portNumber)
	}
	return origin{scheme: scheme, host: host, port: port}, nil
}

func randomToken(reader io.Reader, byteCount int) (string, error) {
	buffer := make([]byte, byteCount)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func constantTimeEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions || method == http.MethodTrace
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func cookieMaxAge(duration time.Duration) int {
	if duration <= 0 {
		return 1
	}
	return int(duration.Seconds())
}

func writeJSON(response http.ResponseWriter, status int, payload any) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(payload)
}

func writeJSONError(response http.ResponseWriter, status int, code string) {
	writeJSON(response, status, struct {
		Error string `json:"error"`
	}{Error: code})
}

func writeSessionError(response http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnauthenticated) {
		writeJSONError(response, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSONError(response, http.StatusInternalServerError, "session_lookup_failed")
}
