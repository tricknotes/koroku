package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// jwtBackdate moves "iat" into the past to tolerate clock drift between
	// this host and GitHub.
	jwtBackdate = 60 * time.Second
	// jwtLifetime is how far "exp" lies in the future. GitHub allows at most 10 minutes.
	jwtLifetime = 9 * time.Minute
	// tokenRefreshMargin is how long before its expiry a cached installation
	// token stops being handed out.
	tokenRefreshMargin = 5 * time.Minute
	// installationCacheTTL is how long a repository's installation ID is remembered.
	installationCacheTTL = 10 * time.Minute
)

// ParsePrivateKey reads the PEM file GitHub issues for an app. Both the PKCS#1
// ("RSA PRIVATE KEY") form GitHub generates and PKCS#8 ("PRIVATE KEY") are
// accepted. Errors never include key material.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github: private key: no PEM block found")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("github: private key: invalid PKCS#1 data")
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("github: private key: invalid PKCS#8 data")
		}
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github: private key: not an RSA key")
		}
		key = rsaKey
	default:
		return nil, fmt.Errorf("github: private key: unsupported PEM block type %.40q", block.Type)
	}
	if err := key.Validate(); err != nil {
		return nil, errors.New("github: private key: key failed validation")
	}
	return key, nil
}

// appJWT signs a short-lived RS256 JWT that authenticates as the app itself.
func (c *Client) appJWT() (string, error) {
	now := c.clock.Now()
	claims, err := json.Marshal(struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}{
		IssuedAt:  now.Add(-jwtBackdate).Unix(),
		ExpiresAt: now.Add(jwtLifetime).Unix(),
		Issuer:    strconv.FormatInt(c.appID, 10),
	})
	if err != nil {
		return "", errors.New("sign app JWT: encode claims")
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("sign app JWT: RSA signature failed")
	}
	return signingInput + "." + enc.EncodeToString(signature), nil
}

// Token is an installation access token. Its value is only reachable through
// Value; printing or logging a Token (even as part of another struct) shows a
// placeholder.
type Token struct {
	// secret is a pointer so reflection-based printers show an address, not the value.
	secret *string
	// ExpiresAt is when GitHub stops accepting the token.
	ExpiresAt time.Time
}

// NewToken wraps a token value. It exists for fakes of this client in other
// packages' tests; the client itself only returns tokens issued by GitHub.
func NewToken(value string, expiresAt time.Time) Token { return newToken(value, expiresAt) }

func newToken(value string, expiresAt time.Time) Token {
	return Token{secret: &value, ExpiresAt: expiresAt}
}

// Value returns the token itself, for use as the HTTP password of
// "x-access-token" or as a bearer credential. Do not log it.
func (t Token) Value() string {
	if t.secret == nil {
		return ""
	}
	return *t.secret
}

// IsZero reports whether the token holds no value.
func (t Token) IsZero() bool { return t.secret == nil || *t.secret == "" }

// String hides the token value.
func (t Token) String() string { return redacted }

// GoString hides the token value from %#v.
func (t Token) GoString() string { return "github.Token{" + redacted + "}" }

// LogValue hides the token value from structured logs.
func (t Token) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText hides the token value from encoders such as encoding/json.
func (t Token) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// tokenSlot caches the token of one installation. gate is a one-element
// semaphore: its holder may read and refresh tok. A channel is used instead of
// a mutex so waiting callers still honor their context.
type tokenSlot struct {
	gate chan struct{}
	tok  Token
}

// installationEntry remembers which installation serves a repository.
type installationEntry struct {
	id        int64
	fetchedAt time.Time
}

// InstallationToken returns an access token for the installation. Tokens are
// cached per installation and reused until five minutes before they expire;
// concurrent callers share a single request to GitHub.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (Token, error) {
	if installationID <= 0 {
		return Token{}, fmt.Errorf("%w: installation id %d", ErrInvalidArgument, installationID)
	}
	c.mu.Lock()
	slot := c.tokens[installationID]
	if slot == nil {
		slot = &tokenSlot{gate: make(chan struct{}, 1)}
		c.tokens[installationID] = slot
	}
	c.mu.Unlock()

	select {
	case slot.gate <- struct{}{}:
	case <-ctx.Done():
		return Token{}, fmt.Errorf("github: installation %d token: %w", installationID, ctx.Err())
	}
	defer func() { <-slot.gate }()

	if !slot.tok.IsZero() && c.clock.Now().Before(slot.tok.ExpiresAt.Add(-tokenRefreshMargin)) {
		return slot.tok, nil
	}
	tok, err := c.createInstallationToken(ctx, installationID)
	if err != nil {
		return Token{}, fmt.Errorf("github: installation %d token: %w", installationID, err)
	}
	slot.tok = tok
	return tok, nil
}

func (c *Client) createInstallationToken(ctx context.Context, installationID int64) (Token, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return Token{}, err
	}
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	_, err = c.do(ctx, call{
		method: http.MethodPost,
		path:   "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens",
		bearer: jwt,
	}, &resp)
	if err != nil {
		return Token{}, err
	}
	if resp.Token == "" || resp.ExpiresAt.IsZero() {
		return Token{}, errors.New("response has no token or expiry")
	}
	return newToken(resp.Token, resp.ExpiresAt), nil
}

// RepoInstallationID asks GitHub which installation of the app covers the
// repository. ErrNotFound means the app is not installed on it.
func (c *Client) RepoInstallationID(ctx context.Context, owner, repo string) (int64, error) {
	if err := validateRepo(owner, repo); err != nil {
		return 0, err
	}
	id, err := c.lookupInstallation(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("github: installation of %s/%s: %w", owner, repo, err)
	}
	c.rememberInstallation(owner, repo, id)
	return id, nil
}

func (c *Client) lookupInstallation(ctx context.Context, owner, repo string) (int64, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return 0, err
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	_, err = c.do(ctx, call{
		method: http.MethodGet,
		path:   repoPath(owner, repo) + "/installation",
		bearer: jwt,
	}, &resp)
	if err != nil {
		return 0, err
	}
	if resp.ID <= 0 {
		return 0, errors.New("response has no installation id")
	}
	return resp.ID, nil
}

// RepoToken returns an installation token that can read the repository. This
// is what the deploy engine hands to git. The repository's installation ID is
// remembered for a few minutes to save a request per call.
func (c *Client) RepoToken(ctx context.Context, owner, repo string) (Token, error) {
	if err := validateRepo(owner, repo); err != nil {
		return Token{}, err
	}
	id, ok := c.cachedInstallation(owner, repo)
	if !ok {
		var err error
		if id, err = c.RepoInstallationID(ctx, owner, repo); err != nil {
			return Token{}, err
		}
	}
	return c.InstallationToken(ctx, id)
}

func installationKey(owner, repo string) string {
	return strings.ToLower(owner + "/" + repo)
}

func (c *Client) cachedInstallation(owner, repo string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.installations[installationKey(owner, repo)]
	if !ok || c.clock.Now().Sub(entry.fetchedAt) >= installationCacheTTL {
		return 0, false
	}
	return entry.id, true
}

func (c *Client) rememberInstallation(owner, repo string, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installations[installationKey(owner, repo)] = installationEntry{id: id, fetchedAt: c.clock.Now()}
}
