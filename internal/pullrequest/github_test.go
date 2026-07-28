package pullrequest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveGitHubTokenPrefersEnvironment(t *testing.T) {
	called := false
	token, err := ResolveGitHubToken(context.Background(),
		func(name string) string {
			assert.Equal(t, "KWT_GITHUB_TOKEN", name)
			return " env-token\n"
		},
		func(context.Context, string, ...string) ([]byte, error) {
			called = true
			return nil, errors.New("must not run")
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "env-token", token)
	assert.False(t, called)
}

func TestResolveGitHubTokenFallsBackToGHWithoutPrompt(t *testing.T) {
	token, err := ResolveGitHubToken(context.Background(), func(string) string { return "" },
		func(_ context.Context, name string, args ...string) ([]byte, error) {
			assert.Equal(t, "gh", name)
			assert.Equal(t, []string{"auth", "token"}, args)
			return []byte("gh-token\n"), nil
		})
	require.NoError(t, err)
	assert.Equal(t, "gh-token", token)
}

func TestResolveGitHubTokenReportsAuthenticationFailureWithoutLeakingOutput(t *testing.T) {
	_, err := ResolveGitHubToken(context.Background(), func(string) string { return "" },
		func(context.Context, string, ...string) ([]byte, error) {
			return []byte("sensitive-test-output"), errors.New("exit 1")
		})

	assertErrorCode(t, err, CodeAuthentication)
	assert.NotContains(t, err.Error(), "sensitive-test-output")
}

func TestGitHubProviderMapsPullRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/acme/widget/pulls", r.URL.Path)
		assert.Equal(t, "all", r.URL.Query().Get("state"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"number":17,"html_url":"https://github.com/acme/widget/pull/17","title":"A draft",`+
			`"user":{"login":"octocat"},"draft":true,"state":"open",`+
			`"head":{"ref":"feature/widgets","sha":"0123456789abcdef0123456789abcdef01234567",`+
			`"repo":{"name":"widget","full_name":"octocat/widget","html_url":"https://github.com/octocat/widget","clone_url":"https://github.com/octocat/widget.git","ssh_url":"git@github.com:octocat/widget.git"}},`+
			`"base":{"ref":"main","repo":{"name":"widget","full_name":"acme/widget","html_url":"https://github.com/acme/widget","clone_url":"https://github.com/acme/widget.git"}}}]`)
	}))
	defer server.Close()

	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithURLs(&baseURL, nil))
	require.NoError(t, err)
	provider := NewGitHubProvider(client)

	prs, err := provider.List(context.Background(), Repository{Provider: "github", Identity: "github.com/acme/widget", Host: "github.com", Owner: "acme", Name: "widget"}, "all")

	require.NoError(t, err)
	require.Len(t, prs, 1)
	pr := prs[0]
	assert.Equal(t, OpaqueID("github.com/acme/widget", 17), pr.ID)
	assert.Equal(t, "octocat", pr.Author)
	assert.True(t, pr.Draft)
	assert.Equal(t, "feature/widgets", pr.Source.Name)
	assert.Equal(t, "github.com/octocat/widget", pr.Source.Repository.Identity)
	assert.Equal(t, "git@github.com:octocat/widget.git", pr.Source.Repository.SSHURL)
	assert.Equal(t, "main", pr.Target.Name)
	assert.Equal(t, "github.com/acme/widget", pr.Repository.Identity)
}

func TestGitHubProviderNormalizesRepositoryIdentityCase(t *testing.T) {
	repository, err := mapGitHubRepository(&github.Repository{
		FullName: github.Ptr("Acme/Widget"), Name: github.Ptr("Widget"),
		CloneURL: github.Ptr("https://github.com/Acme/Widget.git"),
	})

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/widget", repository.Identity)
	assert.Equal(t, "Acme", repository.Owner)
}

func TestGitHubProviderResolvesTransferredRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/legacy/widget", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"name":"widget",
			"full_name":"acme/widget",
			"clone_url":"https://github.com/acme/widget.git",
			"ssh_url":"git@github.com:acme/widget.git"
		}`)
	}))
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithURLs(&baseURL, nil))
	require.NoError(t, err)
	provider := NewGitHubProvider(client)

	repository, err := provider.ResolveRepository(context.Background(), Repository{
		Provider: "github", Identity: "github.com/legacy/widget",
		Host: "github.com", Owner: "legacy", Name: "widget",
	})

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/widget", repository.Identity)
	assert.Equal(t, "acme", repository.Owner)
	assert.Equal(t, "widget", repository.Name)
}

func TestGitHubProviderClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		headers   map[string]string
		want      ErrorCode
		retryable bool
	}{
		{name: "authentication", status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`, want: CodeAuthentication},
		{name: "forbidden", status: http.StatusForbidden, body: `{"message":"forbidden"}`, want: CodeAuthentication},
		{name: "primary rate limit", status: http.StatusForbidden, body: `{"message":"API rate limit exceeded"}`, headers: map[string]string{"X-RateLimit-Remaining": "0"}, want: CodeNetwork, retryable: true},
		{name: "secondary rate limit", status: http.StatusForbidden, body: `{"message":"secondary rate limit"}`, headers: map[string]string{"Retry-After": "60"}, want: CodeNetwork, retryable: true},
		{name: "too many requests", status: http.StatusTooManyRequests, body: `{"message":"slow down"}`, want: CodeNetwork, retryable: true},
		{name: "missing", status: http.StatusNotFound, body: `{"message":"Not Found"}`, want: CodeNotFound},
		{name: "network", status: http.StatusServiceUnavailable, body: `{"message":"try later"}`, want: CodeNetwork, retryable: true},
		{name: "malformed", status: http.StatusOK, body: `{broken`, want: CodeMalformedResponse},
		{name: "wrong JSON types", status: http.StatusOK, body: `[{"number":"seventeen"}]`, want: CodeMalformedResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				for key, value := range tc.headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			baseURL := server.URL + "/"
			client, err := github.NewClient(github.WithURLs(&baseURL, nil))
			require.NoError(t, err)
			provider := NewGitHubProvider(client)

			_, err = provider.List(context.Background(), Repository{Owner: "acme", Name: "widget", Identity: "github.com/acme/widget"}, "open")

			var typed *Error
			require.ErrorAs(t, err, &typed)
			assert.Equal(t, tc.want, typed.Code)
			assert.Equal(t, tc.retryable, typed.Retryable)
		})
	}
}

func TestGitHubProviderListSkipsDeletedHeadRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"number":1,"html_url":"https://github.com/acme/widget/pull/1","title":"gone",`+
			`"user":{"login":"octocat"},"state":"open","head":{"ref":"gone","sha":"0123456789abcdef0123456789abcdef01234567","repo":null},`+
			`"base":{"ref":"main","repo":{"name":"widget","full_name":"acme/widget","clone_url":"https://github.com/acme/widget.git"}}},`+
			`{"number":2,"html_url":"https://github.com/acme/widget/pull/2","title":"available",`+
			`"user":{"login":"hubot"},"state":"closed","head":{"ref":"topic","sha":"0123456789abcdef0123456789abcdef01234567",`+
			`"repo":{"name":"widget","full_name":"hubot/widget","clone_url":"https://github.com/hubot/widget.git"}},`+
			`"base":{"ref":"main","repo":{"name":"widget","full_name":"acme/widget","clone_url":"https://github.com/acme/widget.git"}}}]`)
	}))
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithURLs(&baseURL, nil))
	require.NoError(t, err)
	provider := NewGitHubProvider(client)

	prs, err := provider.List(context.Background(), Repository{Owner: "acme", Name: "widget", Identity: "github.com/acme/widget"}, "all")

	require.NoError(t, err)
	require.Len(t, prs, 1)
	assert.Equal(t, 2, prs[0].Number)
}

func TestGitHubProviderRejectsMalformedSuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"number":1,"head":{},"base":null}]`)
	}))
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithURLs(&baseURL, nil))
	require.NoError(t, err)
	provider := NewGitHubProvider(client)

	_, err = provider.List(context.Background(), Repository{Owner: "acme", Name: "widget", Identity: "github.com/acme/widget"}, "open")

	assertErrorCode(t, err, CodeMalformedResponse)
	assert.True(t, strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "malformed"))
}

func TestMapGitHubPullRequestRejectsInvalidImportFields(t *testing.T) {
	valid := func() *github.PullRequest {
		return &github.PullRequest{
			Number:  github.Ptr(17),
			HTMLURL: github.Ptr("https://github.com/acme/widget/pull/17"),
			Title:   github.Ptr("Improve widgets"),
			User:    &github.User{Login: github.Ptr("octocat")},
			State:   github.Ptr("open"),
			Head: &github.PullRequestBranch{
				Ref: github.Ptr("feature/widgets"),
				SHA: github.Ptr("0123456789abcdef0123456789abcdef01234567"),
				Repo: &github.Repository{
					Name:     github.Ptr("widget"),
					FullName: github.Ptr("octocat/widget"),
					CloneURL: github.Ptr("https://github.com/octocat/widget.git"),
				},
			},
			Base: &github.PullRequestBranch{
				Ref: github.Ptr("main"),
				Repo: &github.Repository{
					Name:     github.Ptr("widget"),
					FullName: github.Ptr("acme/widget"),
					CloneURL: github.Ptr("https://github.com/acme/widget.git"),
				},
			},
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*github.PullRequest)
	}{
		{name: "nonpositive number", mutate: func(pr *github.PullRequest) {
			pr.Number = github.Ptr(0)
		}},
		{name: "empty head ref", mutate: func(pr *github.PullRequest) {
			pr.Head.Ref = github.Ptr("")
		}},
		{name: "empty base ref", mutate: func(pr *github.PullRequest) {
			pr.Base.Ref = github.Ptr("")
		}},
		{name: "invalid head OID", mutate: func(pr *github.PullRequest) {
			pr.Head.SHA = github.Ptr("abc")
		}},
		{name: "empty clone URL", mutate: func(pr *github.PullRequest) {
			pr.Head.Repo.CloneURL = github.Ptr("")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := valid()
			tc.mutate(pr)

			_, err := mapGitHubPullRequest(pr)

			assertErrorCode(t, err, CodeMalformedResponse)
		})
	}
}

func TestGitHubProviderRejectsMismatchedSuccessfulResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		get  bool
	}{
		{
			name: "list repository",
			body: `[{"number":17,"html_url":"https://github.com/other/widget/pull/17","title":"wrong repo",` +
				`"user":{"login":"octocat"},"state":"open",` +
				`"head":{"ref":"topic","sha":"0123456789abcdef0123456789abcdef01234567","repo":{"name":"widget","full_name":"octocat/widget","clone_url":"https://github.com/octocat/widget.git"}},` +
				`"base":{"ref":"main","repo":{"name":"widget","full_name":"other/widget","clone_url":"https://github.com/other/widget.git"}}}]`,
		},
		{
			name: "get repository",
			get:  true,
			body: `{"number":17,"html_url":"https://github.com/other/widget/pull/17","title":"wrong repo",` +
				`"user":{"login":"octocat"},"state":"open",` +
				`"head":{"ref":"topic","sha":"0123456789abcdef0123456789abcdef01234567","repo":{"name":"widget","full_name":"octocat/widget","clone_url":"https://github.com/octocat/widget.git"}},` +
				`"base":{"ref":"main","repo":{"name":"widget","full_name":"other/widget","clone_url":"https://github.com/other/widget.git"}}}`,
		},
		{
			name: "get number",
			get:  true,
			body: `{"number":18,"html_url":"https://github.com/acme/widget/pull/18","title":"wrong number",` +
				`"user":{"login":"octocat"},"state":"open",` +
				`"head":{"ref":"topic","sha":"0123456789abcdef0123456789abcdef01234567","repo":{"name":"widget","full_name":"octocat/widget","clone_url":"https://github.com/octocat/widget.git"}},` +
				`"base":{"ref":"main","repo":{"name":"widget","full_name":"acme/widget","clone_url":"https://github.com/acme/widget.git"}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tc.body)
				},
			))
			defer server.Close()
			baseURL := server.URL + "/"
			client, err := github.NewClient(github.WithURLs(&baseURL, nil))
			require.NoError(t, err)
			provider := NewGitHubProvider(client)
			repository := Repository{
				Owner: "acme", Name: "widget",
				Identity: "github.com/acme/widget",
			}

			if tc.get {
				_, err = provider.Get(context.Background(), repository, 17)
			} else {
				_, err = provider.List(context.Background(), repository, "open")
			}

			assertErrorCode(t, err, CodeMalformedResponse)
		})
	}
}
