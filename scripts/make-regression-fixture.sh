#!/usr/bin/env bash
# Builds the small, deterministic repo used by scripts/regression.sh: a
# realistic 4-file token-refresh change. Keeping the regression target in the
# repo makes the suite self-contained (no external checkout required).
#
# Branches:
#   base             -> before the change
#   refresh-tokens   -> after the change (checked out)
#
# Usage: scripts/make-regression-fixture.sh [out-dir]
set -euo pipefail

OUT="${1:-testdata/regression-repo}"

rm -rf "$OUT"
mkdir -p "$OUT"
cd "$OUT"

git init -q -b main
git config user.email "fixture@example.com"
git config user.name "regression fixture"
git config commit.gpgsign false

mkdir -p internal/auth internal/http internal/config

cat > go.mod <<'EOF'
module example.com/tokenrepo

go 1.22
EOF

# ---- base ----
cat > internal/auth/token.go <<'EOF'
package auth

import (
	"errors"
	"time"
)

type Token struct {
	Value     string
	ExpiresAt time.Time
}

type TokenService struct {
	now func() time.Time
}

func NewTokenService() *TokenService {
	return &TokenService{now: time.Now}
}

func (s *TokenService) Valid(t Token) bool {
	return t.Value != "" && s.now().Before(t.ExpiresAt)
}

func (s *TokenService) Refresh(t Token) (Token, error) {
	if t.Value == "" {
		return Token{}, errors.New("empty token")
	}
	return Token{Value: t.Value, ExpiresAt: s.now().Add(15 * time.Minute)}, nil
}
EOF

cat > internal/auth/token_test.go <<'EOF'
package auth

import (
	"testing"
	"time"
)

func TestValid(t *testing.T) {
	s := NewTokenService()
	if !s.Valid(Token{Value: "abc", ExpiresAt: time.Now().Add(time.Minute)}) {
		t.Fatal("want valid")
	}
}
EOF

cat > internal/http/client.go <<'EOF'
package http

import (
	"context"
	"errors"
	nethttp "net/http"

	"example.com/tokenrepo/internal/auth"
)

var errExpired = errors.New("token expired")

type Client struct {
	tokens *auth.TokenService
	token  auth.Token
	http   *nethttp.Client
}

func New(tokens *auth.TokenService, token auth.Token) *Client {
	return &Client{tokens: tokens, token: token, http: nethttp.DefaultClient}
}

func (c *Client) Do(ctx context.Context, req *nethttp.Request) (*nethttp.Response, error) {
	if !c.tokens.Valid(c.token) {
		return nil, errExpired
	}
	return c.http.Do(req.WithContext(ctx))
}
EOF

cat > internal/config/config.go <<'EOF'
package config

import "os"

type Config struct {
	BaseURL       string
	TokenEndpoint string
}

func Load() Config {
	return Config{
		BaseURL:       os.Getenv("BASE_URL"),
		TokenEndpoint: os.Getenv("TOKEN_ENDPOINT"),
	}
}
EOF

git add -A
GIT_AUTHOR_DATE="2026-01-01T00:00:00Z" GIT_COMMITTER_DATE="2026-01-01T00:00:00Z" \
  git commit -qm "base: token service"
git branch base

# ---- head ----
cat > internal/auth/token.go <<'EOF'
package auth

import (
	"errors"
	"time"
)

var (
	ErrExpired  = errors.New("token expired")
	ErrRefresh  = errors.New("token refresh failed")
	refreshSkew = 30 * time.Second
)

type Token struct {
	Value     string
	ExpiresAt time.Time
}

type TokenService struct {
	now func() time.Time
}

func NewTokenService() *TokenService {
	return &TokenService{now: time.Now}
}

// Valid reports whether the token can still be used. Tokens inside the refresh
// skew window are treated as expired so in-flight requests never race expiry.
func (s *TokenService) Valid(t Token) bool {
	if t.Value == "" {
		return false
	}
	return s.now().Add(refreshSkew).Before(t.ExpiresAt)
}

func (s *TokenService) Refresh(t Token) (Token, error) {
	if t.Value == "" {
		return Token{}, ErrRefresh
	}
	return Token{Value: t.Value, ExpiresAt: s.now().Add(15 * time.Minute)}, nil
}
EOF

cat > internal/auth/token_test.go <<'EOF'
package auth

import (
	"testing"
	"time"
)

func TestValid(t *testing.T) {
	s := NewTokenService()
	if !s.Valid(Token{Value: "abc", ExpiresAt: time.Now().Add(time.Minute)}) {
		t.Fatal("want valid")
	}
}

func TestValidRejectsSkewWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &TokenService{now: func() time.Time { return now }}
	if s.Valid(Token{Value: "abc", ExpiresAt: now.Add(10 * time.Second)}) {
		t.Fatal("token inside refresh skew must be treated as expired")
	}
}

func TestRefreshEmptyToken(t *testing.T) {
	s := NewTokenService()
	if _, err := s.Refresh(Token{}); err != ErrRefresh {
		t.Fatalf("err = %v, want ErrRefresh", err)
	}
}
EOF

cat > internal/http/client.go <<'EOF'
package http

import (
	"context"
	nethttp "net/http"

	"example.com/tokenrepo/internal/auth"
)

type Client struct {
	tokens *auth.TokenService
	token  auth.Token
	http   *nethttp.Client
}

func New(tokens *auth.TokenService, token auth.Token) *Client {
	return &Client{tokens: tokens, token: token, http: nethttp.DefaultClient}
}

// Do refreshes the token once when it is expired, then retries the request so
// callers do not have to handle expiry themselves.
func (c *Client) Do(ctx context.Context, req *nethttp.Request) (*nethttp.Response, error) {
	if !c.tokens.Valid(c.token) {
		refreshed, err := c.tokens.Refresh(c.token)
		if err != nil {
			return nil, err
		}
		c.token = refreshed
	}
	return c.http.Do(req.WithContext(ctx))
}
EOF

cat > internal/config/config.go <<'EOF'
package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	BaseURL       string
	TokenEndpoint string
	Timeout       time.Duration
}

func Load() (Config, error) {
	c := Config{
		BaseURL:       os.Getenv("BASE_URL"),
		TokenEndpoint: os.Getenv("TOKEN_ENDPOINT"),
		Timeout:       30 * time.Second,
	}
	if c.BaseURL == "" {
		return Config{}, fmt.Errorf("BASE_URL is required")
	}
	return c, nil
}
EOF

git add -A
GIT_AUTHOR_DATE="2026-01-01T00:00:01Z" GIT_COMMITTER_DATE="2026-01-01T00:00:01Z" \
  git commit -qm "head: refresh tokens inside the skew window"
git branch refresh-tokens
git checkout -q refresh-tokens

echo "fixture: $(pwd)"
echo "  base...refresh-tokens: $(git diff --shortstat base...refresh-tokens)"
