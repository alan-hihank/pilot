package autopilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alekspetrov/pilot/internal/adapters/github"
)

func newReviewTestServer(t *testing.T, reviews []*github.PullRequestReview, comments []*github.PRReviewComment) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			json.NewEncoder(w).Encode(reviews)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			json.NewEncoder(w).Encode(comments)
			return
		}
		http.NotFound(w, r)
	}))
}

func TestReviewMonitor_CheckReview_NoBotReviews(t *testing.T) {
	server := newReviewTestServer(t,
		[]*github.PullRequestReview{
			{ID: 1, User: github.User{Login: "human-reviewer"}, State: "APPROVED"},
		},
		nil,
	)
	defer server.Close()

	client := github.NewClientWithBaseURL("test-token", server.URL)
	monitor := NewReviewMonitor(client, "owner", "repo", "coderabbitai[bot]", 30*time.Second, 15*time.Minute)

	feedback, err := monitor.CheckReview(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if feedback.Status != ReviewPending {
		t.Errorf("expected ReviewPending, got %s", feedback.Status)
	}
}

func TestReviewMonitor_CheckReview_BotApproved(t *testing.T) {
	server := newReviewTestServer(t,
		[]*github.PullRequestReview{
			{ID: 1, User: github.User{Login: "coderabbitai[bot]"}, State: "APPROVED", Body: "LGTM"},
		},
		[]*github.PRReviewComment{},
	)
	defer server.Close()

	client := github.NewClientWithBaseURL("test-token", server.URL)
	monitor := NewReviewMonitor(client, "owner", "repo", "coderabbitai[bot]", 30*time.Second, 15*time.Minute)

	feedback, err := monitor.CheckReview(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if feedback.Status != ReviewApproved {
		t.Errorf("expected ReviewApproved, got %s", feedback.Status)
	}
}

func TestReviewMonitor_CheckReview_BotChangesRequested(t *testing.T) {
	server := newReviewTestServer(t,
		[]*github.PullRequestReview{
			{ID: 1, User: github.User{Login: "coderabbitai[bot]"}, State: "CHANGES_REQUESTED", Body: "Fix the error handling"},
		},
		[]*github.PRReviewComment{
			{ID: 10, User: github.User{Login: "coderabbitai[bot]"}, Path: "main.go", Line: 42, Body: "Missing nil check"},
		},
	)
	defer server.Close()

	client := github.NewClientWithBaseURL("test-token", server.URL)
	monitor := NewReviewMonitor(client, "owner", "repo", "coderabbitai[bot]", 30*time.Second, 15*time.Minute)

	feedback, err := monitor.CheckReview(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if feedback.Status != ReviewChangesRequested {
		t.Errorf("expected ReviewChangesRequested, got %s", feedback.Status)
	}
	if len(feedback.Comments) != 1 {
		t.Errorf("expected 1 comment, got %d", len(feedback.Comments))
	}
}

func TestReviewMonitor_CheckReview_BotCommentedWithInlineComments(t *testing.T) {
	server := newReviewTestServer(t,
		[]*github.PullRequestReview{
			{ID: 1, User: github.User{Login: "coderabbitai[bot]"}, State: "COMMENTED"},
		},
		[]*github.PRReviewComment{
			{ID: 10, User: github.User{Login: "coderabbitai[bot]"}, Path: "handler.go", Line: 15, Body: "Consider using errors.Is here"},
		},
	)
	defer server.Close()

	client := github.NewClientWithBaseURL("test-token", server.URL)
	monitor := NewReviewMonitor(client, "owner", "repo", "coderabbitai[bot]", 30*time.Second, 15*time.Minute)

	feedback, err := monitor.CheckReview(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// COMMENTED with inline comments should be treated as changes_requested
	if feedback.Status != ReviewChangesRequested {
		t.Errorf("expected ReviewChangesRequested for COMMENTED with inline comments, got %s", feedback.Status)
	}
}

func TestReviewMonitor_CheckReview_UsesLatestReview(t *testing.T) {
	server := newReviewTestServer(t,
		[]*github.PullRequestReview{
			{ID: 1, User: github.User{Login: "coderabbitai[bot]"}, State: "CHANGES_REQUESTED", Body: "Fix issues"},
			{ID: 2, User: github.User{Login: "coderabbitai[bot]"}, State: "APPROVED", Body: "LGTM now"},
		},
		[]*github.PRReviewComment{},
	)
	defer server.Close()

	client := github.NewClientWithBaseURL("test-token", server.URL)
	monitor := NewReviewMonitor(client, "owner", "repo", "coderabbitai[bot]", 30*time.Second, 15*time.Minute)

	feedback, err := monitor.CheckReview(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if feedback.Status != ReviewApproved {
		t.Errorf("expected ReviewApproved (latest review), got %s", feedback.Status)
	}
}

func TestReviewMonitor_FormatReviewPrompt(t *testing.T) {
	monitor := NewReviewMonitor(nil, "owner", "repo", "coderabbitai[bot]", 30*time.Second, 15*time.Minute)

	feedback := &ReviewFeedback{
		Status: ReviewChangesRequested,
		Reviews: []*github.PullRequestReview{
			{Body: "Several issues need fixing", State: "CHANGES_REQUESTED"},
		},
		Comments: []*github.PRReviewComment{
			{Path: "main.go", Line: 10, Body: "Missing error check"},
			{Path: "main.go", Line: 25, Body: "Unused variable"},
			{Path: "handler.go", Line: 5, Body: "Function too long"},
		},
	}

	prompt := monitor.FormatReviewPrompt(feedback, "Add user auth")

	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	for _, expected := range []string{"main.go", "handler.go", "Missing error check", "Unused variable", "Function too long"} {
		if !strings.Contains(prompt, expected) {
			t.Errorf("prompt missing expected content %q", expected)
		}
	}
}
