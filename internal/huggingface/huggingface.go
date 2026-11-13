// Package huggingface is what bh knows about Hugging Face's access tokens:
// where they are made, how to ask whose a token is and what it may do, and
// how to revoke one.
//
// There are no presets here, unlike for GitHub: the page that makes a
// token asks for its kind (read, write, or fine-grained with its own
// choices) clearly enough, and cannot be opened filled in. bh opens it,
// takes the token, and records what Hugging Face says about it.
package huggingface

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

const (
	// TokensURL is where a user makes, refreshes and deletes tokens.
	TokensURL = "https://huggingface.co/settings/tokens"
	// Variable is the name the libraries read the token from.
	Variable = "HF_TOKEN"

	apiURL  = "https://huggingface.co/api"
	maxBody = 1 << 20
)

var tokenPattern = regexp.MustCompile(`^hf_[A-Za-z0-9_-]{8,}$`)

// ValidToken reports whether text has the form of an access token. Whether
// Hugging Face accepts it is another question.
func ValidToken(text string) bool { return tokenPattern.MatchString(text) }

// ErrRejected: Hugging Face does not accept the token.
var ErrRejected = errors.New("Hugging Face does not accept this token")

// Info is what Hugging Face says about a token.
type Info struct {
	// Account is the user the token belongs to.
	Account string
	// Role is the token's kind: read, write or fineGrained. Empty when the
	// answer does not say.
	Role string
	// TokenName is the name the token was given on the page.
	TokenName string
}

// Client talks to the Hugging Face API.
type Client struct {
	HTTP *http.Client
}

func (c Client) do(request *http.Request) (*http.Response, error) {
	request.Header.Set("User-Agent", "bulkhead")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(request)
}

// Inspect asks Hugging Face whose the token is and what it may do.
// ErrRejected means the token is not valid; any other error means the
// question could not be asked or answered, which says nothing about the
// token.
func (c Client) Inspect(ctx context.Context, token string) (Info, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/whoami-v2", nil)
	if err != nil {
		return Info{}, err
	}
	// The token travels in this header only: never in the address.
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.do(request)
	if err != nil {
		return Info{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return Info{}, err
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return Info{}, ErrRejected
	case response.StatusCode < 200 || response.StatusCode > 299:
		return Info{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var answer struct {
		Name string `json:"name"`
		Auth struct {
			AccessToken struct {
				DisplayName string `json:"displayName"`
				Role        string `json:"role"`
			} `json:"accessToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return Info{}, fmt.Errorf("the answer was not understood: %w", err)
	}
	return Info{Account: answer.Name, Role: answer.Auth.AccessToken.Role, TokenName: answer.Auth.AccessToken.DisplayName}, nil
}

// Revoke makes Hugging Face invalidate the token. The endpoint takes no
// authentication (the token is its own proof), answers the same whether or
// not the token existed, and notifies the owner.
func (c Client) Revoke(ctx context.Context, token string) error {
	body, err := json.Marshal(map[string][]string{"credentials": {token}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/credentials/revoke", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxBody))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}
