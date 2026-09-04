package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestGitHubLoginSetsStatePKCEAndRedirectsWithoutRepoScope(t *testing.T) {
	handler := newTestAuthHandler(t, testAuthDeps{})

	request := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	recorder := httptest.NewRecorder()
	handler.handleGitHubLogin(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()

	if response.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusFound)
	}
	authLocation, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse auth location: %v", err)
	}
	authRequest := authLocation
	if authRequest.Query().Get("client_id") != "client-id" {
		t.Fatalf("client_id = %q", authRequest.Query().Get("client_id"))
	}
	if authRequest.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", authRequest.Query().Get("code_challenge_method"))
	}
	if strings.Contains(authRequest.RawQuery, "repo") {
		t.Fatalf("authorization URL requested repo scope: %s", authRequest.RawQuery)
	}

	cookies := cookiesByName(response.Cookies())
	stateCookie := cookies[defaultStateCookie]
	pkceCookie := cookies[defaultPKCECookie]
	if stateCookie == nil || pkceCookie == nil {
		t.Fatalf("state cookie = %v, pkce cookie = %v", stateCookie, pkceCookie)
	}
	if !stateCookie.HttpOnly || !pkceCookie.HttpOnly {
		t.Fatal("transient OAuth cookies must be HttpOnly")
	}
	if stateCookie.Secure || pkceCookie.Secure {
		t.Fatal("loopback development cookies should not be Secure")
	}
	if authRequest.Query().Get("state") != stateCookie.Value {
		t.Fatalf("state in redirect did not match cookie")
	}
	if authRequest.Query().Get("code_challenge") != s256Challenge(pkceCookie.Value) {
		t.Fatalf("code_challenge did not match PKCE verifier cookie")
	}
}

func TestGitHubCallbackCreatesHashedSessionAndMeReturnsUser(t *testing.T) {
	userStore := &fakeUserStore{user: User{ID: "user-1", GitHubID: 123, GitHubLogin: "octo", AvatarURL: "https://example.test/avatar.png"}}
	sessionStore := newFakeSessionStore(userStore.user)
	var tokenSawVerifier string
	var userSawAuthorization string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case "https://oauth.example.test/token":
			if err := request.ParseForm(); err != nil {
				t.Fatalf("parse token form: %v", err)
			}
			tokenSawVerifier = request.Form.Get("code_verifier")
			return jsonResponse(http.StatusOK, `{"access_token":"access-token","token_type":"bearer"}`), nil
		case "https://api.example.test/user":
			userSawAuthorization = request.Header.Get("Authorization")
			return jsonResponse(http.StatusOK, `{"id":123,"login":"octo","avatar_url":"https://example.test/avatar.png","ignored":"ok"}`), nil
		default:
			return jsonResponse(http.StatusNotFound, `{}`), nil
		}
	})

	handler := newTestAuthHandler(t, testAuthDeps{
		tokenURL:       "https://oauth.example.test/token",
		userURL:        "https://api.example.test/user",
		publicURL:      "https://console.example.test",
		secureCookies:  true,
		userStore:      userStore,
		sessionStore:   sessionStore,
		loginRedirect:  "/console",
		roundTripper:   transport,
		randomSeedByte: 30,
	})

	loginRequest := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	loginResponse := httptest.NewRecorder()
	handler.handleGitHubLogin(loginResponse, loginRequest)
	loginCookies := cookiesByName(loginResponse.Result().Cookies())
	stateCookie := loginCookies[defaultStateCookie]
	pkceCookie := loginCookies[defaultPKCECookie]
	if stateCookie == nil || pkceCookie == nil {
		t.Fatalf("state cookie = %v, pkce cookie = %v", stateCookie, pkceCookie)
	}

	callbackRequest := httptest.NewRequest(http.MethodGet, "/auth/github/callback?state="+stateCookie.Value+"&code=valid-code", nil)
	callbackRequest.AddCookie(stateCookie)
	callbackRequest.AddCookie(pkceCookie)
	callbackResponse := httptest.NewRecorder()
	handler.handleGitHubCallback(callbackResponse, callbackRequest)

	if callbackResponse.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d; body=%s", callbackResponse.Code, http.StatusSeeOther, callbackResponse.Body.String())
	}
	if callbackResponse.Header().Get("Location") != "/console" {
		t.Fatalf("redirect location = %q, want /console", callbackResponse.Header().Get("Location"))
	}
	if pkceCookie == nil || tokenSawVerifier != pkceCookie.Value {
		t.Fatalf("token endpoint verifier = %q, want PKCE cookie value", tokenSawVerifier)
	}
	if userSawAuthorization != "Bearer access-token" {
		t.Fatalf("github user authorization = %q", userSawAuthorization)
	}
	if userStore.seen.GitHubID != 123 || userStore.seen.GitHubLogin != "octo" || userStore.seen.AvatarURL == "" {
		t.Fatalf("unexpected GitHub user persisted: %+v", userStore.seen)
	}

	responseCookies := cookiesByName(callbackResponse.Result().Cookies())
	sessionCookie := responseCookies[defaultSessionCookie]
	csrfCookie := responseCookies[defaultCSRFCookie]
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("session cookie = %v, csrf cookie = %v", sessionCookie, csrfCookie)
	}
	if !sessionCookie.HttpOnly || !sessionCookie.Secure {
		t.Fatalf("session cookie flags: HttpOnly=%t Secure=%t", sessionCookie.HttpOnly, sessionCookie.Secure)
	}
	if csrfCookie.HttpOnly || !csrfCookie.Secure {
		t.Fatalf("csrf cookie flags: HttpOnly=%t Secure=%t", csrfCookie.HttpOnly, csrfCookie.Secure)
	}
	if bytes.Contains(sessionStore.created.TokenHash, []byte(sessionCookie.Value)) {
		t.Fatal("store received raw session token instead of a digest")
	}
	if len(sessionStore.created.TokenHash) != sha256.Size {
		t.Fatalf("session token hash length = %d, want %d", len(sessionStore.created.TokenHash), sha256.Size)
	}

	meRequest := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	meRequest.AddCookie(sessionCookie)
	meResponse := httptest.NewRecorder()
	handler.handleMe(meResponse, meRequest)
	if meResponse.Code != http.StatusOK {
		t.Fatalf("me status = %d, want %d; body=%s", meResponse.Code, http.StatusOK, meResponse.Body.String())
	}
	if strings.Contains(meResponse.Body.String(), sessionCookie.Value) {
		t.Fatal("/auth/me response exposed the raw session token")
	}
	var mePayload struct {
		User User `json:"user"`
	}
	if err := json.Unmarshal(meResponse.Body.Bytes(), &mePayload); err != nil {
		t.Fatalf("decode me response: %v", err)
	}
	if mePayload.User.ID != "user-1" || mePayload.User.GitHubLogin != "octo" {
		t.Fatalf("me user = %+v", mePayload.User)
	}
}

func TestGitHubCallbackRejectsInvalidStateBeforeExchange(t *testing.T) {
	var tokenCalled bool
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == "https://oauth.example.test/token" {
			tokenCalled = true
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	})

	handler := newTestAuthHandler(t, testAuthDeps{
		tokenURL:     "https://oauth.example.test/token",
		userURL:      "https://api.example.test/user",
		roundTripper: transport,
	})

	request := httptest.NewRequest(http.MethodGet, "/auth/github/callback?state=attacker&code=secret-code", nil)
	request.AddCookie(&http.Cookie{Name: defaultStateCookie, Value: "real-state"})
	request.AddCookie(&http.Cookie{Name: defaultPKCECookie, Value: "real-verifier"})
	response := httptest.NewRecorder()

	handler.handleGitHubCallback(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if tokenCalled {
		t.Fatal("token endpoint was called for invalid state")
	}
	if strings.Contains(response.Body.String(), "secret-code") {
		t.Fatal("response leaked OAuth code")
	}
	cleared := cookiesByName(response.Result().Cookies())
	if cleared[defaultStateCookie] == nil || cleared[defaultStateCookie].MaxAge != -1 || cleared[defaultPKCECookie] == nil || cleared[defaultPKCECookie].MaxAge != -1 {
		t.Fatal("invalid callback did not consume OAuth state and PKCE cookies")
	}
}

func TestGitHubCallbackExchangeFailureDoesNotLeakCodeOrCreateSession(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == "https://oauth.example.test/token" {
			return jsonResponse(http.StatusBadRequest, `{"error":"upstream rejected bad-code"}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	})

	sessionStore := newFakeSessionStore(User{ID: "user-1"})
	handler := newTestAuthHandler(t, testAuthDeps{
		tokenURL:     "https://oauth.example.test/token",
		userURL:      "https://api.example.test/user",
		sessionStore: sessionStore,
		roundTripper: transport,
	})

	request := httptest.NewRequest(http.MethodGet, "/auth/github/callback?state=real-state&code=bad-code", nil)
	request.AddCookie(&http.Cookie{Name: defaultStateCookie, Value: "real-state"})
	request.AddCookie(&http.Cookie{Name: defaultPKCECookie, Value: "real-verifier"})
	response := httptest.NewRecorder()

	handler.handleGitHubCallback(response, request)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadGateway)
	}
	if sessionStore.createCalls != 0 {
		t.Fatalf("sessions created = %d, want 0", sessionStore.createCalls)
	}
	if strings.Contains(response.Body.String(), "bad-code") || strings.Contains(response.Body.String(), "upstream rejected") {
		t.Fatalf("response leaked sensitive upstream detail: %s", response.Body.String())
	}
}

func TestMeRequiresSession(t *testing.T) {
	handler := newTestAuthHandler(t, testAuthDeps{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	response := httptest.NewRecorder()

	handler.handleMe(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestRegisteredMeRouteUsesAPIPath(t *testing.T) {
	user := User{ID: "user-1", GitHubLogin: "octo"}
	store := newFakeSessionStore(user)
	handler := newTestAuthHandler(t, testAuthDeps{sessionStore: store})
	sessionCookie, _ := seedSession(t, handler, store, user)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	request.AddCookie(sessionCookie)
	response := httptest.NewRecorder()
	handlerRoutes(handler).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
}

func TestLogoutRequiresOriginCSRFAndDeletesSession(t *testing.T) {
	user := User{ID: "user-1", GitHubLogin: "octo"}
	sessionStore := newFakeSessionStore(user)
	handler := newTestAuthHandler(t, testAuthDeps{
		publicURL:     "https://console.example.test",
		secureCookies: true,
		sessionStore:  sessionStore,
	})
	sessionCookie, csrfCookie := seedSession(t, handler, sessionStore, user)

	missingCSRF := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	missingCSRF.Header.Set("Origin", "https://console.example.test")
	missingCSRF.AddCookie(sessionCookie)
	missingCSRFResponse := httptest.NewRecorder()
	handler.RequireSameOrigin(http.HandlerFunc(handler.handleLogout)).ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want %d", missingCSRFResponse.Code, http.StatusForbidden)
	}

	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Origin", "https://console.example.test")
	request.Header.Set("X-CSRF-Token", csrfCookie.Value)
	request.AddCookie(sessionCookie)
	request.AddCookie(csrfCookie)
	response := httptest.NewRecorder()

	handler.RequireSameOrigin(http.HandlerFunc(handler.handleLogout)).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if sessionStore.deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", sessionStore.deleteCalls)
	}
	cleared := cookiesByName(response.Result().Cookies())
	if cleared[defaultSessionCookie] == nil || cleared[defaultSessionCookie].MaxAge != -1 {
		t.Fatalf("session cookie was not cleared: %#v", cleared[defaultSessionCookie])
	}
	if cleared[defaultCSRFCookie] == nil || cleared[defaultCSRFCookie].MaxAge != -1 {
		t.Fatalf("csrf cookie was not cleared: %#v", cleared[defaultCSRFCookie])
	}
}

func TestCanonicalOriginNormalizesCaseDefaultPortsAndLoopback(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
	}{
		{name: "HTTPS host and scheme case", left: "HTTPS://Console.Example.test:443", right: "https://console.example.test"},
		{name: "HTTP default port", left: "HTTP://127.0.0.1:80", right: "http://127.0.0.1"},
		{name: "IPv6 loopback", left: "http://[0:0:0:0:0:0:0:1]:80", right: "http://[::1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left, err := canonicalOrigin(tt.left, false)
			if err != nil {
				t.Fatalf("canonicalize left origin: %v", err)
			}
			right, err := canonicalOrigin(tt.right, false)
			if err != nil {
				t.Fatalf("canonicalize right origin: %v", err)
			}
			if left != right {
				t.Fatalf("origins differ: left=%+v right=%+v", left, right)
			}
		})
	}
}

func TestCanonicalOriginPreservesIPv4MappedIPv6Identity(t *testing.T) {
	ipv4, err := canonicalOrigin("http://127.0.0.1", false)
	if err != nil {
		t.Fatalf("canonicalize IPv4 origin: %v", err)
	}
	mappedIPv6, err := canonicalOrigin("http://[::ffff:127.0.0.1]", false)
	if err != nil {
		t.Fatalf("canonicalize IPv4-mapped IPv6 origin: %v", err)
	}
	if ipv4 == mappedIPv6 {
		t.Fatalf("IPv4 and IPv4-mapped IPv6 origins compare equal: IPv4=%+v mappedIPv6=%+v", ipv4, mappedIPv6)
	}
}

func TestNewAuthHandlerAllowsIPv6LoopbackDevelopmentOrigin(t *testing.T) {
	_ = newTestAuthHandler(t, testAuthDeps{publicURL: "http://[::1]:8081"})
}

func TestRequireSameOriginUsesCanonicalOriginAndRejectsDifferentHost(t *testing.T) {
	handler := newTestAuthHandler(t, testAuthDeps{
		publicURL:     "https://Console.Example.test:443",
		secureCookies: true,
	})

	tests := []struct {
		name       string
		origin     string
		wantStatus int
	}{
		{name: "equivalent canonical origin", origin: "https://console.example.test", wantStatus: http.StatusNoContent},
		{name: "different origin", origin: "https://console.example.test.evil", wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/mutation", nil)
			request.Header.Set("Origin", tt.origin)
			request.Header.Set("X-CSRF-Token", "csrf-token")
			request.AddCookie(&http.Cookie{Name: defaultCSRFCookie, Value: "csrf-token"})
			response := httptest.NewRecorder()

			handler.RequireSameOrigin(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantStatus)
			}
		})
	}
}

func TestLogoutKeepsCookiesWhenSessionDeletionFails(t *testing.T) {
	user := User{ID: "user-1", GitHubLogin: "octo"}
	store := newFakeSessionStore(user)
	handler := newTestAuthHandler(t, testAuthDeps{sessionStore: store})
	sessionCookie, csrfCookie := seedSession(t, handler, store, user)
	store.err = errors.New("database unavailable")

	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Origin", "http://127.0.0.1:8081")
	request.Header.Set("X-CSRF-Token", csrfCookie.Value)
	request.AddCookie(sessionCookie)
	request.AddCookie(csrfCookie)
	response := httptest.NewRecorder()
	handler.RequireSameOrigin(http.HandlerFunc(handler.handleLogout)).ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("logout failure cleared browser cookies while the server session remained active")
	}
}

func TestSessionStoreFailureIsNotReportedAsBadCredentials(t *testing.T) {
	store := newFakeSessionStore(User{ID: "user-1"})
	store.err = errors.New("database unavailable")
	handler := newTestAuthHandler(t, testAuthDeps{sessionStore: store})

	request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	request.AddCookie(&http.Cookie{Name: defaultSessionCookie, Value: "raw-session-token"})
	response := httptest.NewRecorder()
	handler.handleMe(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if strings.Contains(response.Body.String(), "database unavailable") {
		t.Fatal("response exposed database failure detail")
	}
}

func TestNewAuthHandlerRejectsRepoScope(t *testing.T) {
	_, err := NewAuthHandler(AuthConfig{
		OAuth: oauth2.Config{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			Endpoint:     oauth2.Endpoint{AuthURL: "https://github.test/authorize", TokenURL: "https://github.test/token"},
			RedirectURL:  "https://console.example.test/auth/github/callback",
			Scopes:       []string{"repo"},
		},
		PublicConsoleURL:   "https://console.example.test",
		SessionTokenPepper: bytes.Repeat([]byte{1}, 32),
	}, &fakeUserStore{}, newFakeSessionStore(User{}))
	if err == nil {
		t.Fatal("expected repo scope to be rejected")
	}
}

type testAuthDeps struct {
	authURL        string
	tokenURL       string
	userURL        string
	publicURL      string
	loginRedirect  string
	secureCookies  bool
	userStore      *fakeUserStore
	sessionStore   *fakeSessionStore
	roundTripper   http.RoundTripper
	randomSeedByte byte
}

func newTestAuthHandler(t *testing.T, deps testAuthDeps) *AuthHandler {
	t.Helper()
	if deps.authURL == "" {
		deps.authURL = "https://github.example.test/authorize"
	}
	if deps.tokenURL == "" {
		deps.tokenURL = "https://github.example.test/token"
	}
	if deps.userURL == "" {
		deps.userURL = "https://github.example.test/user"
	}
	if deps.publicURL == "" {
		deps.publicURL = "http://127.0.0.1:8081"
	}
	if deps.userStore == nil {
		deps.userStore = &fakeUserStore{user: User{ID: "user-1", GitHubID: 123, GitHubLogin: "octo"}}
	}
	if deps.sessionStore == nil {
		deps.sessionStore = newFakeSessionStore(deps.userStore.user)
	}
	client := &http.Client{Transport: deps.roundTripper}
	if deps.roundTripper == nil {
		client = http.DefaultClient
	}

	handler, err := NewAuthHandler(AuthConfig{
		OAuth: oauth2.Config{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			Endpoint:     oauth2.Endpoint{AuthURL: deps.authURL, TokenURL: deps.tokenURL},
			RedirectURL:  deps.publicURL + "/auth/github/callback",
		},
		GitHubUserURL:      deps.userURL,
		PublicConsoleURL:   deps.publicURL,
		LoginRedirectPath:  deps.loginRedirect,
		SessionTokenPepper: bytes.Repeat([]byte{7}, 32),
		SessionTTL:         time.Hour,
		StateTTL:           time.Minute,
		SecureCookies:      deps.secureCookies,
		HTTPClient:         client,
		Now: func() time.Time {
			return time.Unix(1_800_000_000, 0).UTC()
		},
		Random: bytes.NewReader(bytes.Repeat([]byte{deps.randomSeedByte}, 4096)),
	}, deps.userStore, deps.sessionStore)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}
	return handler
}

func handlerRoutes(handler *AuthHandler) http.Handler {
	mux := http.NewServeMux()
	handler.Register(mux)
	return mux
}

type fakeUserStore struct {
	user User
	seen GitHubUser
	err  error
}

func (s *fakeUserStore) UpsertGitHubUser(_ context.Context, user GitHubUser) (User, error) {
	s.seen = user
	if s.err != nil {
		return User{}, s.err
	}
	if s.user.ID == "" {
		s.user = User{ID: "user-1", GitHubID: user.GitHubID, GitHubLogin: user.GitHubLogin, AvatarURL: user.AvatarURL}
	}
	return s.user, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type fakeSessionStore struct {
	mu          sync.Mutex
	user        User
	sessions    map[string]Session
	created     NewSession
	createCalls int
	deleteCalls int
	err         error
}

func newFakeSessionStore(user User) *fakeSessionStore {
	return &fakeSessionStore{
		user:     user,
		sessions: make(map[string]Session),
	}
}

func (s *fakeSessionStore) CreateSession(_ context.Context, session NewSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	s.created = session
	if s.err != nil {
		return s.err
	}
	s.sessions[string(session.TokenHash)] = Session{User: s.user, ExpiresAt: session.ExpiresAt}
	return nil
}

func (s *fakeSessionStore) GetSession(_ context.Context, tokenHash []byte, now time.Time) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return Session{}, s.err
	}
	session, ok := s.sessions[string(tokenHash)]
	if !ok || !session.ExpiresAt.After(now) {
		return Session{}, ErrNotFound
	}
	return session, nil
}

func (s *fakeSessionStore) DeleteSession(_ context.Context, tokenHash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	if s.err != nil && !errors.Is(s.err, ErrNotFound) {
		return s.err
	}
	delete(s.sessions, string(tokenHash))
	return nil
}

func seedSession(t *testing.T, handler *AuthHandler, store *fakeSessionStore, user User) (*http.Cookie, *http.Cookie) {
	t.Helper()
	sessionToken := "raw-session-token"
	csrfToken := "csrf-token"
	expires := handler.now().Add(time.Hour)
	if err := store.CreateSession(context.Background(), NewSession{
		UserID:    user.ID,
		TokenHash: handler.sessionTokenHash(sessionToken),
		ExpiresAt: expires,
		Now:       handler.now(),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return handler.sessionCookie(sessionToken, expires), handler.csrfCookie(csrfToken, expires)
}

func cookiesByName(cookies []*http.Cookie) map[string]*http.Cookie {
	byName := make(map[string]*http.Cookie, len(cookies))
	for _, cookie := range cookies {
		byName[cookie.Name] = cookie
	}
	return byName
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
