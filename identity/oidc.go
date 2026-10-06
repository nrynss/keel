package identity

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The OpenID Connect provider runs the authorization code flow with
// state, nonce and PKCE. The start builds the provider redirect from the
// issuer's discovery document. The callback exchange proves the PKCE
// verifier, and the token check pins issuer, audience, expiry, nonce,
// signature and subject before the subject may resolve. The identity key
// is the provider subject, never an address.
//
// oidcMaxBody caps every provider answer, so a large response never
// reaches the decoder.
const oidcMaxBody = 1 << 20

// OIDCConfig configures an OpenID Connect provider. Issuer is the
// provider origin, such as the public origin the provider documents. The
// client pair comes from the provider's console, and RedirectURL is the
// callback that console registered. HTTPClient calls the provider, and
// nil means a client with a thirty second ceiling. Now stamps token
// expiry checks, and nil means time.Now.
type OIDCConfig struct {
	// Issuer is the provider origin the tokens must cite.
	Issuer string
	// ClientID is the public OAuth client the audience must cite.
	ClientID string
	// ClientSecret authenticates the code exchange.
	ClientSecret string
	// RedirectURL is the registered callback URL.
	RedirectURL string
	// HTTPClient calls the provider endpoints.
	HTTPClient *http.Client
	// Now supplies the clock the token expiry reads. Nil means time.Now.
	Now func() time.Time
}

// OIDCProvider signs visitors in through any OpenID Connect provider
// that serves discovery, RS256 signing keys and the authorization code
// flow with PKCE. Create it with NewOIDCProvider. An OIDCProvider is
// safe for concurrent use.
type OIDCProvider struct {
	cfg OIDCConfig
	now func() time.Time

	// discovered caches the issuer document, because the endpoints never
	// move while the process runs. A failed read leaves the cache empty.
	// Guarded by mu.
	mu         sync.Mutex
	discovered *oidcDiscovery
}

// NewOIDCProvider validates cfg and returns the provider. Every field
// but the client and the clock must be set, because a half wired flow
// would redirect nowhere useful.
func NewOIDCProvider(cfg OIDCConfig) (*OIDCProvider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, fmt.Errorf("%w: issuer, client and redirect are required", ErrInvalid)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &OIDCProvider{cfg: cfg, now: now}, nil
}

// Name is the provider name the identity rows store.
func (p *OIDCProvider) Name() string { return p.cfg.Issuer }

// oidcDiscovery carries the provider endpoints. The flow reads them from
// the issuer once per process, so no address is baked into code.
type oidcDiscovery struct {
	// Issuer must echo the configured issuer exactly.
	Issuer string `json:"issuer"`
	// AuthURL starts the authorization redirect.
	AuthURL string `json:"authorization_endpoint"`
	// TokenURL exchanges the code.
	TokenURL string `json:"token_endpoint"`
	// KeysURL serves the signing keys.
	KeysURL string `json:"jwks_uri"`
}

// oidcTokenJSON carries the code exchange answer. Only the token
// matters, and its absence refuses the flow.
type oidcTokenJSON struct {
	// IDToken is the signed token the flow validates.
	IDToken string `json:"id_token"`
}

// oidcHeader carries the token header. Only RSA with SHA-256 passes, so
// key confusion through another algorithm stops here.
type oidcHeader struct {
	// Alg must read RS256.
	Alg string `json:"alg"`
	// Kid selects the signing key.
	Kid string `json:"kid"`
}

// oidcKey carries one signing key. The flow builds the RSA key from the
// modulus and the exponent on every validation.
type oidcKey struct {
	// KTY must read RSA.
	KTY string `json:"kty"`
	// KID selects this key from its set.
	KID string `json:"kid"`
	// N is the base64url modulus.
	N string `json:"n"`
	// E is the base64url exponent.
	E string `json:"e"`
}

// oidcKeySet carries the provider signing keys.
type oidcKeySet struct {
	// Keys holds the active signing keys.
	Keys []oidcKey `json:"keys"`
}

// oidcAudience reads the token audience as one string or many. The
// provider sends one, and the shape accepts both either way.
type oidcAudience []string

// UnmarshalJSON reads one audience string or a list of them.
func (a *oidcAudience) UnmarshalJSON(raw []byte) error {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		*a = oidcAudience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return err
	}
	*a = oidcAudience(many)
	return nil
}

// oidcClaims carries the validated token fields. The address never
// appears here, because the subject alone keys the identity.
type oidcClaims struct {
	// Issuer must echo the configured issuer.
	Issuer string `json:"iss"`
	// Audience must cite the client id.
	Audience oidcAudience `json:"aud"`
	// Subject keys the identity.
	Subject string `json:"sub"`
	// Expiry bounds the token in Unix seconds.
	Expiry int64 `json:"exp"`
	// Nonce must echo the pending nonce.
	Nonce string `json:"nonce"`
}

// client returns the provider caller. Tests pass their own, and the
// default bounds every call well under the request ceiling.
func (p *OIDCProvider) client() *http.Client {
	if p.cfg.HTTPClient != nil {
		return p.cfg.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// discovery reads the issuer document, once per process, and checks it
// cites the configured issuer. A document for another issuer stops here,
// which pins the flow to the provider the boot named.
func (p *OIDCProvider) discovery(ctx context.Context) (oidcDiscovery, error) {
	p.mu.Lock()
	cached := p.discovered
	p.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	var doc oidcDiscovery
	location := strings.TrimRight(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	if err := fetchOIDCJSON(ctx, p.client(), location, &doc); err != nil {
		return oidcDiscovery{}, err
	}
	if doc.Issuer != p.cfg.Issuer || doc.AuthURL == "" || doc.TokenURL == "" || doc.KeysURL == "" {
		return oidcDiscovery{}, fmt.Errorf("%w: provider discovery is incomplete", ErrProviderToken)
	}
	p.mu.Lock()
	p.discovered = &doc
	p.mu.Unlock()
	return doc, nil
}

// fetchOIDCJSON reads one provider document up to the cap. Any
// transport fault, refusal, or malformed body maps to the token
// sentinel, so no provider wording ever reaches the caller.
func fetchOIDCJSON(ctx context.Context, client *http.Client, location string, shape any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return fmt.Errorf("%w: fetch provider document: %v", ErrProviderToken, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: fetch provider document: %v", ErrProviderToken, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: provider document refused", ErrProviderToken)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oidcMaxBody+1))
	if err != nil {
		return fmt.Errorf("%w: read provider document: %v", ErrProviderToken, err)
	}
	if len(raw) > oidcMaxBody {
		return fmt.Errorf("%w: provider document is too large", ErrProviderToken)
	}
	if err := json.Unmarshal(raw, shape); err != nil {
		return fmt.Errorf("%w: decode provider document: %v", ErrProviderToken, err)
	}
	return nil
}

// AuthorizeURL builds the provider redirect for one start. Only the
// OpenID scope travels, because identity is the one capability the flow
// asks for.
func (p *OIDCProvider) AuthorizeURL(ctx context.Context, start StartChallenge) (string, error) {
	doc, err := p.discovery(ctx)
	if err != nil {
		return "", err
	}
	query := url.Values{}
	query.Set("client_id", p.cfg.ClientID)
	query.Set("redirect_uri", p.cfg.RedirectURL)
	query.Set("response_type", "code")
	query.Set("scope", "openid")
	query.Set("state", start.State)
	query.Set("nonce", start.Nonce)
	query.Set("code_challenge", oidcChallenge(start.Verifier))
	query.Set("code_challenge_method", "S256")
	return doc.AuthURL + "?" + query.Encode(), nil
}

// oidcChallenge derives the PKCE challenge from its verifier with
// SHA-256. The provider checks the exchange against this value.
func oidcChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Exchange trades one callback code for its token and validates the
// answer. The verifier proves the exchange belongs to the start that
// holds it, and the token checks pin issuer, audience, expiry, nonce,
// signature and subject before the subject may resolve.
func (p *OIDCProvider) Exchange(ctx context.Context, callback CallbackChallenge) (string, error) {
	doc, err := p.discovery(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", callback.Code)
	form.Set("client_id", p.cfg.ClientID)
	form.Set("client_secret", p.cfg.ClientSecret)
	form.Set("redirect_uri", p.cfg.RedirectURL)
	form.Set("code_verifier", callback.Verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: exchange code: %v", ErrProviderToken, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: exchange code: %v", ErrProviderToken, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: token endpoint refused", ErrProviderToken)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oidcMaxBody+1))
	if err != nil {
		return "", fmt.Errorf("%w: read token answer: %v", ErrProviderToken, err)
	}
	if len(raw) > oidcMaxBody {
		return "", fmt.Errorf("%w: token answer is too large", ErrProviderToken)
	}
	var answer oidcTokenJSON
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", fmt.Errorf("%w: decode token answer: %v", ErrProviderToken, err)
	}
	if answer.IDToken == "" {
		return "", fmt.Errorf("%w: token answer carries no token", ErrProviderToken)
	}
	return p.checkToken(ctx, callback.Nonce, answer.IDToken)
}

// oidcKeyFor returns the signing key one token header names. A rotation
// between the fetch and the check refetches once, so a fresh key never
// fails a valid token.
func (p *OIDCProvider) oidcKeyFor(ctx context.Context, keysURL, kid string) (oidcKey, error) {
	var set oidcKeySet
	if err := fetchOIDCJSON(ctx, p.client(), keysURL, &set); err != nil {
		return oidcKey{}, err
	}
	for _, key := range set.Keys {
		if key.KID == kid {
			return key, nil
		}
	}
	set = oidcKeySet{}
	if err := fetchOIDCJSON(ctx, p.client(), keysURL, &set); err != nil {
		return oidcKey{}, err
	}
	for _, key := range set.Keys {
		if key.KID == kid {
			return key, nil
		}
	}
	return oidcKey{}, fmt.Errorf("%w: signing key is unknown", ErrProviderToken)
}

// oidcPublicKey builds one RSA key from its set entry. Anything but RSA
// stops here, which keeps elliptic or octet keys out of the check.
func oidcPublicKey(key oidcKey) (*rsa.PublicKey, error) {
	if key.KTY != "RSA" || key.N == "" || key.E == "" {
		return nil, fmt.Errorf("%w: signing key is not RSA", ErrProviderToken)
	}
	modulus, err := base64.RawURLEncoding.DecodeString(key.N)
	if err != nil {
		return nil, fmt.Errorf("%w: decode signing key: %v", ErrProviderToken, err)
	}
	exponent, err := base64.RawURLEncoding.DecodeString(key.E)
	if err != nil {
		return nil, fmt.Errorf("%w: decode signing key: %v", ErrProviderToken, err)
	}
	exp := new(big.Int).SetBytes(exponent)
	if !exp.IsInt64() || exp.Int64() <= 0 || exp.Int64() > 1<<31-1 {
		return nil, fmt.Errorf("%w: signing key exponent is invalid", ErrProviderToken)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exp.Int64())}, nil
}

// checkToken validates one token and returns its subject. It checks the
// issuer, the audience, the expiry, the nonce, the RSA signature and the
// subject, and every refusal shares the one token sentinel.
func (p *OIDCProvider) checkToken(ctx context.Context, nonce, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: token shape is invalid", ErrProviderToken)
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("%w: decode token header: %v", ErrProviderToken, err)
	}
	var header oidcHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return "", fmt.Errorf("%w: decode token header: %v", ErrProviderToken, err)
	}
	if header.Alg != "RS256" || header.Kid == "" {
		return "", fmt.Errorf("%w: token algorithm is not RS256", ErrProviderToken)
	}
	doc, err := p.discovery(ctx)
	if err != nil {
		return "", err
	}
	key, err := p.oidcKeyFor(ctx, doc.KeysURL, header.Kid)
	if err != nil {
		return "", err
	}
	pub, err := oidcPublicKey(key)
	if err != nil {
		return "", err
	}
	signed := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signed))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("%w: decode token signature: %v", ErrProviderToken, err)
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return "", fmt.Errorf("%w: token signature is invalid: %v", ErrProviderToken, err)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("%w: decode token claims: %v", ErrProviderToken, err)
	}
	var claims oidcClaims
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return "", fmt.Errorf("%w: decode token claims: %v", ErrProviderToken, err)
	}
	if claims.Issuer != p.cfg.Issuer {
		return "", fmt.Errorf("%w: token issuer is wrong", ErrProviderToken)
	}
	heard := false
	for _, aud := range claims.Audience {
		if aud == p.cfg.ClientID {
			heard = true
			break
		}
	}
	if !heard {
		return "", fmt.Errorf("%w: token audience is wrong", ErrProviderToken)
	}
	if claims.Expiry <= p.now().Unix() {
		return "", fmt.Errorf("%w: token is expired", ErrProviderToken)
	}
	if claims.Nonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1 {
		return "", fmt.Errorf("%w: token nonce is wrong", ErrProviderToken)
	}
	if claims.Subject == "" {
		return "", fmt.Errorf("%w: token carries no subject", ErrProviderToken)
	}
	return claims.Subject, nil
}
