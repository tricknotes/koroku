package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// User is the GitHub account behind a user access token.
type User struct {
	ID        int64
	Login     string
	AvatarURL string
	// TwoFactor is whether the account has two-factor authentication
	// enabled, or nil when GitHub did not say (the field is only part of
	// the "private user" response).
	TwoFactor *bool
}

// AuthorizeURL returns the address the browser is sent to in order to start
// the web application flow. state is the caller's CSRF token; redirectURI may
// be empty to use the callback registered for the app.
func (c *Client) AuthorizeURL(state, redirectURI string) string {
	q := url.Values{}
	q.Set("client_id", c.clientID)
	q.Set("state", state)
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	return c.webBase + "/login/oauth/authorize?" + q.Encode()
}

// ExchangeCode trades the code GitHub passed to the callback for a user access
// token. The token is meant to be used once (see User) and never stored.
// Rejections by GitHub are returned as *OAuthError, which matches ErrUnauthorized.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (userToken string, err error) {
	if code == "" {
		return "", fmt.Errorf("%w: empty authorization code", ErrInvalidArgument)
	}
	form := url.Values{}
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("code", code)
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	// The escaped forms cover an upstream that echoes the raw request body.
	secrets := []string{c.clientSecret, code, url.QueryEscape(c.clientSecret), url.QueryEscape(code)}

	// GitHub reports a rejected exchange with HTTP 200 and an "error" field.
	var resp struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_, err = c.do(ctx, call{
		method:  http.MethodPost,
		web:     true,
		path:    "/login/oauth/access_token",
		form:    form,
		secrets: secrets,
	}, &resp)
	if err != nil {
		return "", fmt.Errorf("github: exchange authorization code: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("github: exchange authorization code: %w", &OAuthError{
			Code:        sanitize(resp.Error, secrets),
			Description: sanitize(resp.ErrorDescription, secrets),
		})
	}
	if resp.AccessToken == "" {
		return "", errors.New("github: exchange authorization code: response has no access token")
	}
	return resp.AccessToken, nil
}

// User returns the account a user access token belongs to. The caller compares
// the ID with the allowed operator ID.
func (c *Client) User(ctx context.Context, userToken string) (User, error) {
	if userToken == "" {
		return User{}, fmt.Errorf("%w: empty user token", ErrInvalidArgument)
	}
	var resp struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
		TwoFactor *bool  `json:"two_factor_authentication"`
	}
	_, err := c.do(ctx, call{
		method: http.MethodGet,
		path:   "/user",
		bearer: userToken,
	}, &resp)
	if err != nil {
		return User{}, fmt.Errorf("github: get authenticated user: %w", err)
	}
	if resp.ID <= 0 {
		return User{}, errors.New("github: get authenticated user: response has no user id")
	}
	return User{ID: resp.ID, Login: resp.Login, AvatarURL: resp.AvatarURL, TwoFactor: resp.TwoFactor}, nil
}
