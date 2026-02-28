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

// mockReviewFixExecutor implements ReviewFixExecutor for testing.
type mockReviewFixExecutor struct {
	runFunc func(ctx context.Context, projectPath, branch, prompt string) (string, error)
}

func (m *mockReviewFixExecutor) RunReviewFix(ctx context.Context, projectPath, branch, prompt string) (string, error) {
	if m.runFunc != nil {
		return m.runFunc(ctx, projectPath, branch, prompt)
	}
	return "abc1234", nil
}

func TestReviewFixer_FixReviewComments(t *testing.T) {
	var postedComment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Capture PR comment
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/comments") {
			var body struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			postedComment = body.Body
			json.NewEncoder(w).Encode(github.PRComment{ID: 1, Body: body.Body})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := github.NewClientWithBaseURL("test-token", server.URL)
	executor := &mockReviewFixExecutor{}

	fixer := NewReviewFixer(client, executor, "owner", "repo")

	prState := &PRState{
		PRNumber:            42,
		BranchName:          "pilot/GH-10",
		PRTitle:             "Add user auth",
		ReviewFixIterations: 1,
	}

	feedback := &ReviewFeedback{
		Status: ReviewChangesRequested,
		Reviews: []*github.PullRequestReview{
			{Body: "Fix error handling", State: "CHANGES_REQUESTED"},
		},
		Comments: []*github.PRReviewComment{
			{Path: "main.go", Line: 10, Body: "Missing nil check"},
		},
	}

	monitor := NewReviewMonitor(nil, "owner", "repo", "coderabbitai[bot]", 30*time.Second, 15*time.Minute)

	err := fixer.FixReviewComments(context.Background(), prState, feedback, "/tmp/project", monitor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify SHA was updated
	if prState.HeadSHA != "abc1234" {
		t.Errorf("HeadSHA = %s, want abc1234", prState.HeadSHA)
	}

	// Verify summary comment was posted
	if postedComment == "" {
		t.Error("expected summary comment to be posted")
	}
	if !strings.Contains(postedComment, "iteration 1") {
		t.Errorf("comment should mention iteration, got: %s", postedComment)
	}
}

func TestReviewFixer_FixReviewComments_ExecutorError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := github.NewClientWithBaseURL("test-token", server.URL)
	executor := &mockReviewFixExecutor{
		runFunc: func(ctx context.Context, projectPath, branch, prompt string) (string, error) {
			return "", context.DeadlineExceeded
		},
	}

	fixer := NewReviewFixer(client, executor, "owner", "repo")

	prState := &PRState{
		PRNumber:   42,
		BranchName: "pilot/GH-10",
	}

	feedback := &ReviewFeedback{
		Status:   ReviewChangesRequested,
		Comments: []*github.PRReviewComment{},
	}

	monitor := NewReviewMonitor(nil, "owner", "repo", "coderabbitai[bot]", 30*time.Second, 15*time.Minute)

	err := fixer.FixReviewComments(context.Background(), prState, feedback, "/tmp/project", monitor)
	if err == nil {
		t.Fatal("expected error from executor failure")
	}
	if !strings.Contains(err.Error(), "review fix execution failed") {
		t.Errorf("expected 'review fix execution failed' error, got: %v", err)
	}
}

func TestReviewFixer_BuildSummaryComment(t *testing.T) {
	fixer := &ReviewFixer{}

	feedback := &ReviewFeedback{
		Reviews: []*github.PullRequestReview{
			{Body: "Fix issues", State: "CHANGES_REQUESTED"},
		},
		Comments: []*github.PRReviewComment{
			{Path: "a.go", Line: 1, Body: "fix1"},
			{Path: "b.go", Line: 2, Body: "fix2"},
			{Path: "c.go", Line: 3, Body: "fix3"},
		},
	}

	comment := fixer.buildSummaryComment(feedback, 2)

	if !strings.Contains(comment, "iteration 2") {
		t.Errorf("expected iteration 2 in comment, got: %s", comment)
	}
	if !strings.Contains(comment, "3 inline comment(s)") {
		t.Errorf("expected '3 inline comment(s)' in comment, got: %s", comment)
	}
	if !strings.Contains(comment, "1 review summary comment(s)") {
		t.Errorf("expected '1 review summary comment(s)' in comment, got: %s", comment)
	}
}
