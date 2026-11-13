// Package githubtest provides a stand-in for GitHub's API, for tests of the
// code that calls it: an http.RoundTripper that answers from memory, so
// that no test reaches the network.
package githubtest

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Request is a request that was made.
type Request struct {
	Method string
	URL    string
	// Authorization is the header of that name.
	Authorization string
	Body          string
}

// Path is the path of the request's address.
func (r Request) Path() string {
	path := strings.TrimPrefix(r.URL, "https://api.github.com")
	path = strings.TrimPrefix(path, "https://huggingface.co")
	path, _, _ = strings.Cut(path, "?")
	return path
}

// Responder answers a request.
type Responder func(*http.Request) (*http.Response, error)

// Web answers requests to api.github.com by path. A request to anywhere
// else, or to a path nothing was set up for, fails the test.
type Web struct {
	t      testing.TB
	mu     sync.Mutex
	routes map[string]Responder
	made   []Request
}

// New returns a Web that knows no paths yet.
func New(t testing.TB) *Web { return &Web{t: t, routes: map[string]Responder{}} }

// Handle sets what answers requests to the path.
func (w *Web) Handle(path string, respond Responder) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.routes[path] = respond
}

// Reply answers requests to the path with a status and a body. headers are
// name, value pairs.
func (w *Web) Reply(path string, status int, body string, headers ...string) {
	w.Handle(path, func(*http.Request) (*http.Response, error) {
		return Response(status, body, headers...), nil
	})
}

// Fail makes requests to the path fail as if the network did.
func (w *Web) Fail(path string, err error) {
	w.Handle(path, func(*http.Request) (*http.Response, error) { return nil, err })
}

// Response makes a response.
func Response(status int, body string, headers ...string) *http.Response {
	header := http.Header{}
	for i := 0; i+1 < len(headers); i += 2 {
		header.Set(headers[i], headers[i+1])
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

// Requests are the requests made so far, in order.
func (w *Web) Requests() []Request {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Request{}, w.made...)
}

// To are the requests made to the path.
func (w *Web) To(path string) []Request {
	var matching []Request
	for _, request := range w.Requests() {
		if request.Path() == path {
			matching = append(matching, request)
		}
	}
	return matching
}

// RoundTrip implements http.RoundTripper.
func (w *Web) RoundTrip(request *http.Request) (*http.Response, error) {
	body := ""
	if request.Body != nil {
		data, _ := io.ReadAll(request.Body)
		request.Body.Close()
		body = string(data)
	}
	made := Request{
		Method: request.Method, URL: request.URL.String(),
		Authorization: request.Header.Get("Authorization"), Body: body,
	}
	w.mu.Lock()
	w.made = append(w.made, made)
	respond := w.routes[request.URL.Path]
	w.mu.Unlock()

	// The services bh talks to: GitHub's API, and Hugging Face (whose paths
	// all start with /api, so the two do not collide).
	known := request.URL.Host == "api.github.com" || request.URL.Host == "huggingface.co"
	if request.URL.Scheme != "https" || !known || respond == nil {
		w.t.Errorf("unexpected request: %s %s", request.Method, request.URL)
		return nil, errors.New("unexpected request")
	}
	response, err := respond(request)
	if response != nil {
		response.Request = request
	}
	return response, err
}

// Accept makes GitHub accept a token: /user answers with the login (and
// headers, as name, value pairs), and the token reaches no repositories.
func (w *Web) Accept(login string, headers ...string) {
	w.Reply("/user", http.StatusOK, `{"login": "`+login+`"}`, headers...)
	w.Reply("/user/repos", http.StatusOK, `[]`)
}

// Revocable makes the revocation endpoint accept.
func (w *Web) Revocable() { w.Reply("/credentials/revoke", http.StatusAccepted, "") }

// HuggingFace makes Hugging Face accept a token of the role, as the
// account's, and take revocations.
func (w *Web) HuggingFace(account, role string) {
	w.Reply("/api/whoami-v2", http.StatusOK,
		`{"type": "user", "name": "`+account+`", "auth": {"type": "access_token", "accessToken": {"displayName": "bh", "role": "`+role+`"}}}`)
	w.Reply("/api/credentials/revoke", http.StatusAccepted, "")
}
