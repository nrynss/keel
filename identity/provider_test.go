package identity_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/identity"
	"github.com/nrynss/keel/wire"
)

// fakeOIDC serves the provider endpoints one external sign-in test
// needs. It signs its own tokens with an in-test key, serves its own
// discovery document and key set, and checks the PKCE verifier against
// the challenge the start sent. No packet leaves the loopback.
type fakeOIDC struct {
	t         *testing.T
	key       *rsa.PrivateKey
	signKey   *rsa.PrivateKey
	kid       string
	server    *httptest.Server
	mu        sync.Mutex
	code      string
	challenge string
	claims    map[string]any
	verifier  string
	calls     int
}

// openFakeOIDC starts one provider with a fresh signing key.
func openFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	fake := &fakeOIDC{t: t, key: key, kid: "test-key", code: "good-code"}
	fake.signKey = key
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", fake.serveDiscovery)
	mux.HandleFunc("/keys", fake.serveKeys)
	mux.HandleFunc("/token", fake.serveToken)
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// base returns the provider origin the config cites as its issuer.
func (f *fakeOIDC) base() string { return f.server.URL }

// config builds the provider config pointing at this fake.
func (f *fakeOIDC) config(clock *testClock) identity.OIDCConfig {
	return identity.OIDCConfig{
		Issuer:       f.base(),
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://localhost:8080/api/sign-in/provider/callback",
		HTTPClient:   f.server.Client(),
		Now:          clock.now,
	}
}

// jwkPublic renders the test key as a set entry.
func (f *fakeOIDC) jwkPublic() map[string]string {
	pub := f.key.Public().(*rsa.PublicKey)
	return map[string]string{
		"kty": "RSA",
		"kid": f.kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// serveDiscovery answers the issuer document citing this server.
func (f *fakeOIDC) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"issuer":                 f.base(),
		"authorization_endpoint": f.base() + "/auth",
		"token_endpoint":         f.base() + "/token",
		"jwks_uri":               f.base() + "/keys",
	})
}

// serveKeys answers the key set with the test key.
func (f *fakeOIDC) serveKeys(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{f.jwkPublic()}})
}

// mint signs one token with the given claims.
func (f *fakeOIDC) mint(claims map[string]any) string {
	f.t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": f.kid, "typ": "JWT"})
	if err != nil {
		f.t.Fatalf("encode token header: %v", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatalf("encode token claims: %v", err)
	}
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.signKey, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatalf("sign token: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// serveToken checks the code and the PKCE verifier, then answers one
// signed token. A mismatch refuses, which proves the exchange binds the
// start that holds the verifier.
func (f *fakeOIDC) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.verifier = r.Form.Get("code_verifier")
	if r.Form.Get("code") != f.code || r.Form.Get("grant_type") != "authorization_code" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"refused"}`))
		return
	}
	if f.verifier == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"refused"}`))
		return
	}
	if f.challenge != "" {
		sum := sha256.Sum256([]byte(f.verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"refused"}`))
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id_token": f.mint(f.claims), "token_type": "Bearer"})
}

// setClaims stores the claims the next mint signs.
func (f *fakeOIDC) setClaims(claims map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = claims
}

// standardClaims builds live claims for one subject and one nonce. The
// expiry reads one hour past the test clock, so only the expired case
// overrides it.
func standardClaims(issuer, client, subject, nonce string, exp int64) map[string]any {
	return map[string]any{
		"iss": issuer, "aud": client, "sub": subject,
		"exp": exp, "iat": exp - 3600, "nonce": nonce,
	}
}

// providerFixture carries the base fixture with one provider wired.
type providerFixture struct {
	*fixture
	fake *fakeOIDC
}

// openProvider opens the base fixture with a fake provider whose claims
// name one subject.
func openProvider(t *testing.T, clock *testClock, subject string) *providerFixture {
	t.Helper()
	base := openFixture(t, clock)
	fake := openFakeOIDC(t)
	fake.setClaims(standardClaims(fake.base(), "test-client-id", subject, "pending", clock.now().Add(time.Hour).Unix()))
	return &providerFixture{fixture: base, fake: fake}
}

// providerHandler wraps the provider routes in the middleware, the way
// an app mounts them.
func (fx *providerFixture) providerHandler() (http.Handler, error) {
	provider, err := identity.NewOIDCProvider(fx.fake.config(fx.clock))
	if err != nil {
		return nil, err
	}
	return fx.svc.Middleware(fx.svc.ProviderSignInHandler(
		"/api/sign-in/provider/start", "/api/sign-in/provider/callback", provider)), nil
}

// startFlow runs the start route and returns the redirect target with
// the state, the nonce and the challenge it carries.
func (fx *providerFixture) startFlow(cookie *http.Cookie) (state, nonce, challenge string) {
	fx.t.Helper()
	handler, err := fx.providerHandler()
	if err != nil {
		fx.t.Fatalf("build provider: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/sign-in/provider/start", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		fx.t.Fatalf("start = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	target, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		fx.t.Fatalf("parse redirect: %v", err)
	}
	query := target.Query()
	if got := query.Get("scope"); got != "openid" {
		fx.t.Fatalf("scope = %q, want openid only", got)
	}
	return query.Get("state"), query.Get("nonce"), query.Get("code_challenge")
}

// completeFlow runs the callback route with the values a test names.
func (fx *providerFixture) completeFlow(cookie *http.Cookie, state, code, choice string) *httptest.ResponseRecorder {
	fx.t.Helper()
	handler, err := fx.providerHandler()
	if err != nil {
		fx.t.Fatalf("build provider: %v", err)
	}
	target := "/api/sign-in/provider/callback?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(code)
	if choice != "" {
		target += "&choice=" + url.QueryEscape(choice)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// armChallenge pins the PKCE check in the fake to the challenge one
// start carried.
func (fx *providerFixture) armChallenge(challenge string) {
	fx.fake.mu.Lock()
	defer fx.fake.mu.Unlock()
	fx.fake.challenge = challenge
}

// armClaims stores the claims the next mint signs, with the nonce one
// start carried.
func (fx *providerFixture) armClaims(nonce string) map[string]any {
	fx.fake.setClaims(standardClaims(fx.fake.base(), "test-client-id", "sub-123", nonce,
		fx.clock.now().Add(time.Hour).Unix()))
	return fx.fake.claims
}

// TestProviderStartBuildsTheRedirect checks the start redirect carries
// the state, the nonce and the PKCE challenge, and asks for the OpenID
// scope only.
func TestProviderStartBuildsTheRedirect(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openProvider(t, clock, "sub-123")
	cookie, _ := fx.mint()
	state, nonce, challenge := fx.startFlow(cookie)
	if len(state) < 20 || len(nonce) < 20 || len(challenge) < 40 {
		t.Fatalf("start drew state %q, nonce %q, challenge %q, want long random values",
			state, nonce, challenge)
	}
}

// TestProviderStartRefusesWithoutConfig checks a half wired provider
// refuses to build a redirect.
func TestProviderStartRefusesWithoutConfig(t *testing.T) {
	_, err := identity.NewOIDCProvider(identity.OIDCConfig{Issuer: "https://provider.example"})
	if err == nil {
		t.Fatal("provider with no client or redirect built, want an error")
	}
}

// TestProviderCallbackAttachesSubjectOnFirstUse checks the callback
// exchanges the code under the pinned PKCE challenge, validates the
// token, and attaches the provider subject to the current guest.
func TestProviderCallbackAttachesSubjectOnFirstUse(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openProvider(t, clock, "sub-123")
	cookie, user := fx.mint()
	state, nonce, challenge := fx.startFlow(cookie)
	fx.armChallenge(challenge)
	fx.armClaims(nonce)
	rec := fx.completeFlow(cookie, state, fx.fake.code, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("callback = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if fx.cookie(rec) == nil {
		t.Fatal("callback set no rotated cookie")
	}
	fx.fake.mu.Lock()
	calls, verifier := fx.fake.calls, fx.fake.verifier
	fx.fake.mu.Unlock()
	if calls != 1 || verifier == "" {
		t.Fatalf("exchange ran %d times with verifier %q, want one call proving the verifier", calls, verifier)
	}
	var kind, holder string
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT kind FROM users WHERE id = ?", user.ID).Scan(&kind); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if kind != identity.KindOwner {
		t.Fatalf("kind = %q, want %q", kind, identity.KindOwner)
	}
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT user_id FROM identities WHERE provider = ? AND subject = 'sub-123'",
		fx.fake.base()).Scan(&holder); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if holder != user.ID {
		t.Fatalf("identity holds %q, want the guest id %q", holder, user.ID)
	}
	// A replayed callback meets an unknown state instead of a second
	// sign-in.
	replay := fx.completeFlow(cookie, state, fx.fake.code, "")
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed callback = %d, want 401", replay.Code)
	}
}

// TestProviderCallbackRefusesUnknownState checks a callback with no
// usable start stops before any provider call.
func TestProviderCallbackRefusesUnknownState(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openProvider(t, clock, "sub-123")
	cookie, _ := fx.mint()
	rec := fx.completeFlow(cookie, "unknown-state", fx.fake.code, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown state = %d, want 401", rec.Code)
	}
	fx.fake.mu.Lock()
	calls := fx.fake.calls
	fx.fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("the exchange ran %d times, want none on an unknown state", calls)
	}
}

// TestProviderCallbackChecksTheToken runs every token guard: nonce,
// audience, expiry, issuer and signature. Every refusal answers 401 and
// attaches nothing.
func TestProviderCallbackChecksTheToken(t *testing.T) {
	cases := []struct {
		name   string
		claims func(fx *providerFixture, nonce string) map[string]any
	}{
		{
			name: "wrong nonce",
			claims: func(fx *providerFixture, _ string) map[string]any {
				return standardClaims(fx.fake.base(), "test-client-id", "sub-123", "forged-nonce",
					fx.clock.now().Add(time.Hour).Unix())
			},
		},
		{
			name: "wrong audience",
			claims: func(fx *providerFixture, nonce string) map[string]any {
				return standardClaims(fx.fake.base(), "other-client", "sub-123", nonce,
					fx.clock.now().Add(time.Hour).Unix())
			},
		},
		{
			name: "expired token",
			claims: func(fx *providerFixture, nonce string) map[string]any {
				return standardClaims(fx.fake.base(), "test-client-id", "sub-123", nonce,
					fx.clock.now().Add(-time.Minute).Unix())
			},
		},
		{
			name: "wrong issuer",
			claims: func(fx *providerFixture, nonce string) map[string]any {
				return standardClaims("https://other.example", "test-client-id", "sub-123", nonce,
					fx.clock.now().Add(time.Hour).Unix())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{at: time.Unix(1758000000, 0)}
			fx := openProvider(t, clock, "sub-123")
			cookie, user := fx.mint()
			state, nonce, challenge := fx.startFlow(cookie)
			fx.armChallenge(challenge)
			fx.fake.setClaims(tc.claims(fx, nonce))
			rec := fx.completeFlow(cookie, state, fx.fake.code, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: callback = %d, want 401: %s", tc.name, rec.Code, rec.Body.String())
			}
			var refused struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
				t.Fatalf("%s: decode refusal: %v", tc.name, err)
			}
			if refused.Error.Code != wire.CodeInvalidCode {
				t.Fatalf("%s: refusal code = %q, want %q", tc.name, refused.Error.Code, wire.CodeInvalidCode)
			}
			var identities int
			if err := fx.db.Reader().QueryRowContext(t.Context(),
				"SELECT COUNT(*) FROM identities").Scan(&identities); err != nil {
				t.Fatalf("%s: count identities: %v", tc.name, err)
			}
			if identities != 0 {
				t.Fatalf("%s: a refused token attached %d identities", tc.name, identities)
			}
			if _, ok := identity.UserFromContext(context.Background()); ok {
				t.Fatal("a bare context carried a user")
			}
			_ = user
		})
	}
}

// TestProviderCallbackRefusesABadSignature checks a token signed by
// another key refuses, even with every claim right.
func TestProviderCallbackRefusesABadSignature(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openProvider(t, clock, "sub-123")
	cookie, _ := fx.mint()
	state, nonce, challenge := fx.startFlow(cookie)
	fx.armChallenge(challenge)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	fx.fake.mu.Lock()
	fx.fake.signKey = other
	fx.fake.mu.Unlock()
	fx.fake.setClaims(standardClaims(fx.fake.base(), "test-client-id", "sub-123", nonce,
		clock.now().Add(time.Hour).Unix()))
	if rec := fx.completeFlow(cookie, state, fx.fake.code, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged token = %d, want 401", rec.Code)
	}
}

// TestProviderCallbackRefusesABadCode checks a callback code the
// provider never issued refuses.
func TestProviderCallbackRefusesABadCode(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openProvider(t, clock, "sub-123")
	cookie, _ := fx.mint()
	state, nonce, challenge := fx.startFlow(cookie)
	fx.armChallenge(challenge)
	fx.armClaims(nonce)
	if rec := fx.completeFlow(cookie, state, "wrong-code", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad code = %d, want 401", rec.Code)
	}
}

// TestProviderCallbackFlagsConflictThenSwitches checks a guest with app
// data gets the conflict code with nothing changed, and the switch
// choice retries the same state without a second code.
func TestProviderCallbackFlagsConflictThenSwitches(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openProvider(t, clock, "sub-123")
	// The provider subject already belongs to the account, through the
	// sign-in the account holder ran from another device.
	accountCookie, _ := fx.mint()
	state, nonce, challenge := fx.startFlow(accountCookie)
	fx.armChallenge(challenge)
	fx.armClaims(nonce)
	if rec := fx.completeFlow(accountCookie, state, fx.fake.code, ""); rec.Code != http.StatusOK {
		fx.t.Fatalf("account callback = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	cookie, guest := fx.mint()
	fx.ownsData = true
	state, nonce, challenge = fx.startFlow(cookie)
	fx.armChallenge(challenge)
	fx.armClaims(nonce)
	conflict := fx.completeFlow(cookie, state, fx.fake.code, "")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict callback = %d, want 409: %s", conflict.Code, conflict.Body.String())
	}
	var refused struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(conflict.Body.Bytes(), &refused); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if refused.Error.Code != wire.CodeGuestDataConflict {
		t.Fatalf("refusal code = %q, want %q", refused.Error.Code, wire.CodeGuestDataConflict)
	}
	if fx.fake.claims["sub"] == "" {
		t.Fatal("the fake lost its subject")
	}
	// The provider code is single use, so the switch retry carries the
	// same state with no code.
	move := fx.completeFlow(cookie, state, "", "switch")
	if move.Code != http.StatusOK {
		t.Fatalf("switch callback = %d, want 200: %s", move.Code, move.Body.String())
	}
	rotated := fx.cookie(move)
	if rotated == nil {
		t.Fatal("switch callback set no rotated cookie")
	}
	var holder string
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT user_id FROM sessions WHERE id = ?",
		strings.SplitN(rotated.Value, ".", 2)[0]).Scan(&holder); err != nil {
		t.Fatalf("read rotated session: %v", err)
	}
	if holder == guest.ID {
		t.Fatal("switch choice left the device on the guest")
	}
}

// TestProviderSubjectKeysTheIdentity checks the identity row holds the
// provider subject and never an address, even when the token carries
// one.
func TestProviderSubjectKeysTheIdentity(t *testing.T) {
	clock := &testClock{at: time.Unix(1758000000, 0)}
	fx := openProvider(t, clock, "subject-id-42")
	cookie, user := fx.mint()
	state, nonce, challenge := fx.startFlow(cookie)
	fx.armChallenge(challenge)
	claims := standardClaims(fx.fake.base(), "test-client-id", "subject-id-42", nonce,
		clock.now().Add(time.Hour).Unix())
	claims["email"] = "subject-id-42@example.com"
	fx.fake.setClaims(claims)
	if rec := fx.completeFlow(cookie, state, fx.fake.code, ""); rec.Code != http.StatusOK {
		t.Fatalf("callback = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var provider, subject string
	if err := fx.db.Reader().QueryRowContext(t.Context(),
		"SELECT provider, subject FROM identities WHERE user_id = ?", user.ID).Scan(&provider, &subject); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if subject != "subject-id-42" {
		t.Fatalf("identity subject = %q, want the provider subject", subject)
	}
	if provider != fx.fake.base() {
		t.Fatalf("identity provider = %q, want the configured issuer", provider)
	}
	if strings.Contains(subject, "@") {
		t.Fatal("the identity key is an address, want the provider subject")
	}
}
