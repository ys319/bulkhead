package huggingface_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ys319/bulkhead/internal/github/githubtest"
	"github.com/ys319/bulkhead/internal/huggingface"
)

func TestInspectAndRevoke(t *testing.T) {
	web := githubtest.New(t)
	client := huggingface.Client{HTTP: &http.Client{Transport: web}}
	web.HuggingFace("me", "write")

	info, err := client.Inspect(context.Background(), "hf_abcdefgh")
	if err != nil || info.Account != "me" || info.Role != "write" || info.TokenName != "bh" {
		t.Errorf("info = %+v, %v", info, err)
	}
	asked := web.To("/api/whoami-v2")
	if len(asked) != 1 || asked[0].Authorization != "Bearer hf_abcdefgh" || strings.Contains(asked[0].URL, "hf_") {
		t.Errorf("the question: %+v", asked)
	}

	if err := client.Revoke(context.Background(), "hf_abcdefgh"); err != nil {
		t.Fatal(err)
	}
	revoked := web.To("/api/credentials/revoke")
	if len(revoked) != 1 || revoked[0].Body != `{"credentials":["hf_abcdefgh"]}` || revoked[0].Authorization != "" {
		t.Errorf("the revocation: %+v", revoked)
	}

	// A token that is not valid is told from a question that failed.
	web.Reply("/api/whoami-v2", http.StatusUnauthorized, `{"error": "Invalid credentials"}`)
	if _, err := client.Inspect(context.Background(), "hf_abcdefgh"); !errors.Is(err, huggingface.ErrRejected) {
		t.Errorf("a rejected token: %v", err)
	}
	web.Reply("/api/whoami-v2", http.StatusBadGateway, "")
	if _, err := client.Inspect(context.Background(), "hf_abcdefgh"); err == nil || errors.Is(err, huggingface.ErrRejected) {
		t.Errorf("a failed question: %v", err)
	}
	// An answer in another shape still says the token is good.
	web.Reply("/api/whoami-v2", http.StatusOK, `{"name": "me"}`)
	if info, err := client.Inspect(context.Background(), "hf_abcdefgh"); err != nil || info.Role != "" || info.Account != "me" {
		t.Errorf("a sparse answer: %+v, %v", info, err)
	}
}

func TestValidToken(t *testing.T) {
	for text, want := range map[string]bool{"hf_abcDEF12345": true, "hf_": false, "github_pat_x": false, "hf_with space": false, "": false} {
		if got := huggingface.ValidToken(text); got != want {
			t.Errorf("ValidToken(%q) = %v", text, got)
		}
	}
}
