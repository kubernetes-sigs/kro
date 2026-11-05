package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestNewGitHubClientURLs(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	for _, tt := range []struct {
		name          string
		baseURL       string
		wantBaseURL   string
		wantUploadURL string
	}{
		{
			name:          "public",
			baseURL:       defaultAPIBaseURL,
			wantBaseURL:   "https://api.github.com/",
			wantUploadURL: "https://uploads.github.com/",
		},
		{
			name:          "public with trailing slashes",
			baseURL:       "https://api.github.com///",
			wantBaseURL:   "https://api.github.com/",
			wantUploadURL: "https://api.github.com/",
		},
		{
			name:          "enterprise host",
			baseURL:       "https://github.example.com",
			wantBaseURL:   "https://github.example.com/api/v3/",
			wantUploadURL: "https://github.example.com/api/uploads/",
		},
		{
			name:          "enterprise API path with trailing slashes",
			baseURL:       "https://github.example.com/api/v3///",
			wantBaseURL:   "https://github.example.com/api/v3/",
			wantUploadURL: "https://github.example.com/api/v3/api/uploads/",
		},
		{
			name:          "enterprise path prefix",
			baseURL:       "https://github.example.com/github/",
			wantBaseURL:   "https://github.example.com/github/api/v3/",
			wantUploadURL: "https://github.example.com/github/api/uploads/",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, err := newGitHubClient(crawlConfig{APIBaseURL: tt.baseURL})
			if err != nil {
				t.Fatalf("configure client: %v", err)
			}

			request, err := client.client.NewRequest(t.Context(), http.MethodGet, "repos/owner/repo/pulls/42", nil)
			if err != nil {
				t.Fatalf("create API request: %v", err)
			}
			if got, want := request.URL.String(), tt.wantBaseURL+"repos/owner/repo/pulls/42"; got != want {
				t.Errorf("API request URL = %q, want %q", got, want)
			}

			upload, err := client.client.NewUploadRequest(t.Context(), "repos/owner/repo/releases/1/assets", nil, 0, "")
			if err != nil {
				t.Fatalf("create upload request: %v", err)
			}
			if got, want := upload.URL.String(), tt.wantUploadURL+"repos/owner/repo/releases/1/assets"; got != want {
				t.Errorf("upload request URL = %q, want %q", got, want)
			}
		})
	}
}

func TestNewGitHubClientHTTP(t *testing.T) {
	for _, tt := range []struct {
		name              string
		githubToken       string
		ghToken           string
		wantAuthorization string
	}{
		{
			name: "unauthenticated",
		},
		{
			name:        "whitespace tokens",
			githubToken: " \t\n",
			ghToken:     " \t\n",
		},
		{
			name:              "GITHUB_TOKEN precedence",
			githubToken:       " github-token\n",
			ghToken:           "gh-token",
			wantAuthorization: "Bearer github-token",
		},
		{
			name:              "GH_TOKEN fallback",
			githubToken:       " \t\n",
			ghToken:           " gh-token\n",
			wantAuthorization: "Bearer gh-token",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tt.githubToken)
			t.Setenv("GH_TOKEN", tt.ghToken)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.URL.Path, "/api/v3/repos/owner/repo/pulls/42"; got != want {
					t.Errorf("request path = %q, want %q", got, want)
				}
				if got := r.Header.Get("Authorization"); got != tt.wantAuthorization {
					t.Errorf("Authorization = %q, want %q", got, tt.wantAuthorization)
				}
				if got := r.Header.Get("User-Agent"); got != "kro-krep-crawl" {
					t.Errorf("User-Agent = %q, want kro-krep-crawl", got)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]int{"number": 42}); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			cfg := crawlConfig{APIBaseURL: server.URL, Timeout: 5 * time.Second}
			client, err := newGitHubClient(cfg)
			if err != nil {
				t.Fatalf("configure client: %v", err)
			}
			if got := client.client.Client().Timeout; got != cfg.Timeout {
				t.Errorf("HTTP timeout = %v, want %v", got, cfg.Timeout)
			}

			pr, err := client.getPullRequest(t.Context(), "owner", "repo", 42)
			if err != nil {
				t.Fatalf("get pull request: %v", err)
			}
			if got := pr.GetNumber(); got != 42 {
				t.Errorf("pull request number = %d, want 42", got)
			}
		})
	}
}

func TestNewGitHubClientInvalidURL(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	client, err := newGitHubClient(crawlConfig{APIBaseURL: "https://[invalid"})
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Errorf("constructor error = %v, want a URL parsing error", err)
	}
	if client.client != nil {
		t.Error("client must be nil when construction fails")
	}
}
