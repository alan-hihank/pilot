package autopilot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/alekspetrov/pilot/internal/adapters/github"
)

// ReviewStatus represents the current state of bot review on a PR.
type ReviewStatus string

const (
	// ReviewPending indicates no bot review has been submitted yet.
	ReviewPending ReviewStatus = "pending"
	// ReviewApproved indicates the bot approved the PR.
	ReviewApproved ReviewStatus = "approved"
	// ReviewChangesRequested indicates the bot requested changes.
	ReviewChangesRequested ReviewStatus = "changes_requested"
	// ReviewCommented indicates the bot left comments without explicit approval/rejection.
	ReviewCommented ReviewStatus = "commented"
)

// ReviewFeedback aggregates bot review comments for a PR.
type ReviewFeedback struct {
	Status   ReviewStatus
	Reviews  []*github.PullRequestReview
	Comments []*github.PRReviewComment
}

// ReviewMonitor polls for bot review comments on PRs.
// It filters reviews by a configurable bot login (e.g., "coderabbitai[bot]").
type ReviewMonitor struct {
	ghClient     *github.Client
	owner        string
	repo         string
	botLogin     string
	pollInterval time.Duration
	waitTimeout  time.Duration
	log          *slog.Logger
}

// NewReviewMonitor creates a ReviewMonitor configured for the given bot.
func NewReviewMonitor(ghClient *github.Client, owner, repo, botLogin string, pollInterval, waitTimeout time.Duration) *ReviewMonitor {
	return &ReviewMonitor{
		ghClient:     ghClient,
		owner:        owner,
		repo:         repo,
		botLogin:     botLogin,
		pollInterval: pollInterval,
		waitTimeout:  waitTimeout,
		log:          slog.Default().With("component", "review-monitor"),
	}
}

// CheckReview performs a single non-blocking check for bot reviews on a PR.
// Returns the current review status and any review feedback.
func (m *ReviewMonitor) CheckReview(ctx context.Context, prNumber int) (*ReviewFeedback, error) {
	reviews, err := m.ghClient.ListPullRequestReviews(ctx, m.owner, m.repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("failed to list reviews: %w", err)
	}

	// Filter for bot reviews only
	var botReviews []*github.PullRequestReview
	for _, r := range reviews {
		if m.isBotReview(r) {
			botReviews = append(botReviews, r)
		}
	}

	if len(botReviews) == 0 {
		return &ReviewFeedback{Status: ReviewPending}, nil
	}

	// Determine overall status from most recent bot review
	latest := botReviews[len(botReviews)-1]
	status := m.reviewStateToStatus(latest.State)

	// Get inline comments from the bot
	allComments, err := m.ghClient.GetPullRequestComments(ctx, m.owner, m.repo, prNumber)
	if err != nil {
		m.log.Warn("failed to get PR review comments", "pr", prNumber, "error", err)
		// Non-fatal: proceed without inline comments
	}

	var botComments []*github.PRReviewComment
	for _, c := range allComments {
		if m.isBotComment(c) {
			botComments = append(botComments, c)
		}
	}

	// If bot left inline comments but only "COMMENTED" state, treat as changes requested
	if status == ReviewCommented && len(botComments) > 0 {
		status = ReviewChangesRequested
	}

	return &ReviewFeedback{
		Status:   status,
		Reviews:  botReviews,
		Comments: botComments,
	}, nil
}

// isBotReview checks if a review was submitted by the configured bot.
func (m *ReviewMonitor) isBotReview(r *github.PullRequestReview) bool {
	return strings.EqualFold(r.User.Login, m.botLogin)
}

// isBotComment checks if a review comment was submitted by the configured bot.
func (m *ReviewMonitor) isBotComment(c *github.PRReviewComment) bool {
	return strings.EqualFold(c.User.Login, m.botLogin)
}

// reviewStateToStatus maps GitHub review states to our ReviewStatus.
func (m *ReviewMonitor) reviewStateToStatus(state string) ReviewStatus {
	switch strings.ToUpper(state) {
	case "APPROVED":
		return ReviewApproved
	case "CHANGES_REQUESTED":
		return ReviewChangesRequested
	case "COMMENTED":
		return ReviewCommented
	case "DISMISSED":
		return ReviewPending
	default:
		return ReviewPending
	}
}

// FormatReviewPrompt builds a structured prompt from bot review comments
// that can be passed to Claude Code to fix the issues.
func (m *ReviewMonitor) FormatReviewPrompt(feedback *ReviewFeedback, prTitle string) string {
	var b strings.Builder

	b.WriteString("Fix the following code review comments from the automated reviewer:\n\n")

	// Include top-level review bodies
	for _, r := range feedback.Reviews {
		if r.Body != "" && strings.ToUpper(r.State) != "APPROVED" {
			b.WriteString("## Review Summary\n")
			b.WriteString(r.Body)
			b.WriteString("\n\n")
		}
	}

	// Include inline comments grouped by file
	if len(feedback.Comments) > 0 {
		commentsByFile := make(map[string][]*github.PRReviewComment)
		for _, c := range feedback.Comments {
			commentsByFile[c.Path] = append(commentsByFile[c.Path], c)
		}

		b.WriteString("## Inline Comments\n\n")
		for path, comments := range commentsByFile {
			b.WriteString(fmt.Sprintf("### File: `%s`\n", path))
			for _, c := range comments {
				if c.Line > 0 {
					b.WriteString(fmt.Sprintf("- **Line %d**: %s\n", c.Line, c.Body))
				} else {
					b.WriteString(fmt.Sprintf("- %s\n", c.Body))
				}
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("Important: Only fix the issues mentioned above. Do not make unrelated changes.\n")
	b.WriteString("After fixing, commit and push the changes to the same branch.\n")

	return b.String()
}
