package autopilot

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/alekspetrov/pilot/internal/adapters/github"
)

// ReviewFixExecutor is the interface for running Claude Code to fix review comments.
// It abstracts the executor so the review fixer can be tested independently.
type ReviewFixExecutor interface {
	// RunReviewFix executes Claude Code with a review fix prompt on the given branch.
	// Returns the new commit SHA after fixes are pushed, or an error.
	RunReviewFix(ctx context.Context, projectPath, branch, prompt string) (commitSHA string, err error)
}

// ReviewFixer applies fixes for bot review comments by invoking Claude Code.
type ReviewFixer struct {
	ghClient *github.Client
	executor ReviewFixExecutor
	owner    string
	repo     string
	log      *slog.Logger
}

// NewReviewFixer creates a ReviewFixer.
func NewReviewFixer(ghClient *github.Client, executor ReviewFixExecutor, owner, repo string) *ReviewFixer {
	return &ReviewFixer{
		ghClient: ghClient,
		executor: executor,
		owner:    owner,
		repo:     repo,
		log:      slog.Default().With("component", "review-fixer"),
	}
}

// FixReviewComments runs Claude Code to address review feedback, pushes fixes,
// and posts a summary comment on the PR.
func (f *ReviewFixer) FixReviewComments(ctx context.Context, prState *PRState, feedback *ReviewFeedback, projectPath string, monitor *ReviewMonitor) error {
	f.log.Info("fixing review comments",
		"pr", prState.PRNumber,
		"comments", len(feedback.Comments),
		"iteration", prState.ReviewFixIterations,
	)

	// Build the fix prompt from review feedback
	prompt := monitor.FormatReviewPrompt(feedback, prState.PRTitle)

	// Run Claude Code to fix the issues
	newSHA, err := f.executor.RunReviewFix(ctx, projectPath, prState.BranchName, prompt)
	if err != nil {
		return fmt.Errorf("review fix execution failed: %w", err)
	}

	// Update PR state with new SHA
	if newSHA != "" {
		prState.HeadSHA = newSHA
	}

	// Post summary comment on PR
	commentBody := f.buildSummaryComment(feedback, prState.ReviewFixIterations)
	if _, err := f.ghClient.AddPRComment(ctx, f.owner, f.repo, prState.PRNumber, commentBody); err != nil {
		f.log.Warn("failed to post review fix summary comment", "pr", prState.PRNumber, "error", err)
		// Non-fatal: the fixes are already pushed
	}

	return nil
}

// buildSummaryComment creates a markdown comment summarizing what was fixed.
func (f *ReviewFixer) buildSummaryComment(feedback *ReviewFeedback, iteration int) string {
	commentCount := len(feedback.Comments)
	reviewCount := 0
	for _, r := range feedback.Reviews {
		if r.Body != "" {
			reviewCount++
		}
	}

	body := fmt.Sprintf("## Autopilot: Review Fix (iteration %d)\n\n", iteration)
	body += fmt.Sprintf("Addressed %d inline comment(s)", commentCount)
	if reviewCount > 0 {
		body += fmt.Sprintf(" and %d review summary comment(s)", reviewCount)
	}
	body += " from automated reviewer.\n\n"
	body += "Fixes have been pushed to this branch. Awaiting new CI run and re-review.\n"

	return body
}
