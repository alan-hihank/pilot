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
	// ReviewApproved indicates the bot approved the PR (no actionable comments).
	ReviewApproved ReviewStatus = "approved"
	// ReviewChangesRequested indicates the bot left actionable comments.
	ReviewChangesRequested ReviewStatus = "changes_requested"
	// ReviewCommented indicates the bot left comments without explicit approval/rejection.
	ReviewCommented ReviewStatus = "commented"
)

// ReviewFeedback aggregates bot review comments for a PR.
type ReviewFeedback struct {
	Status   ReviewStatus
	Reviews  []*github.PullRequestReview
	Comments []*github.PRReviewComment
	// IssueComments are top-level PR comments (e.g., CodeRabbit summary).
	IssueComments []*github.Comment
}

// ReviewMonitor polls for bot review comments on PRs.
// It filters reviews by a configurable bot login (e.g., "coderabbitai[bot]").
// Supports bots like CodeRabbit that use PR comments instead of formal GitHub reviews.
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
// Supports two modes:
//  1. Formal GitHub reviews (approve/request changes) — used by bots that submit reviews.
//  2. Comment-based reviews (CodeRabbit style) — checks PR comments and inline review comments.
//
// CodeRabbit posts a summary comment with "No actionable comments" when clean,
// or leaves inline review comments when there are issues to fix.
func (m *ReviewMonitor) CheckReview(ctx context.Context, prNumber int) (*ReviewFeedback, error) {
	// Strategy 1: Check formal GitHub reviews from the bot
	reviews, err := m.ghClient.ListPullRequestReviews(ctx, m.owner, m.repo, prNumber)
	if err != nil {
		m.log.Warn("failed to list reviews, falling back to comment-based check", "pr", prNumber, "error", err)
	}

	var botReviews []*github.PullRequestReview
	for _, r := range reviews {
		if m.isBotReview(r) {
			botReviews = append(botReviews, r)
		}
	}

	// If the bot submitted formal reviews, use those
	if len(botReviews) > 0 {
		latest := botReviews[len(botReviews)-1]
		status := m.reviewStateToStatus(latest.State)

		inlineComments := m.getInlineComments(ctx, prNumber)

		if status == ReviewCommented && len(inlineComments) > 0 {
			status = ReviewChangesRequested
		}

		return &ReviewFeedback{
			Status:   status,
			Reviews:  botReviews,
			Comments: inlineComments,
		}, nil
	}

	// Strategy 2: Comment-based review (CodeRabbit style)
	// Check for bot's summary comment on the PR
	issueComments, err := m.ghClient.ListIssueComments(ctx, m.owner, m.repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("failed to list PR comments: %w", err)
	}

	var botSummaryComments []*github.Comment
	for _, c := range issueComments {
		if strings.EqualFold(c.User.Login, m.botLogin) {
			botSummaryComments = append(botSummaryComments, c)
		}
	}

	if len(botSummaryComments) == 0 {
		m.log.Debug("no bot comments found yet", "pr", prNumber, "bot", m.botLogin)
		return &ReviewFeedback{Status: ReviewPending}, nil
	}

	// Check for inline review comments from the bot
	inlineComments := m.getInlineComments(ctx, prNumber)

	// Determine status from the latest summary comment
	latestSummary := botSummaryComments[len(botSummaryComments)-1]
	status := m.classifySummaryComment(latestSummary.Body, inlineComments)

	m.log.Info("bot review status determined",
		"pr", prNumber,
		"status", status,
		"summary_comments", len(botSummaryComments),
		"inline_comments", len(inlineComments),
	)

	return &ReviewFeedback{
		Status:        status,
		Comments:      inlineComments,
		IssueComments: botSummaryComments,
	}, nil
}

// getInlineComments fetches inline review comments from the bot.
func (m *ReviewMonitor) getInlineComments(ctx context.Context, prNumber int) []*github.PRReviewComment {
	allComments, err := m.ghClient.GetPullRequestComments(ctx, m.owner, m.repo, prNumber)
	if err != nil {
		m.log.Warn("failed to get PR review comments", "pr", prNumber, "error", err)
		return nil
	}

	var botComments []*github.PRReviewComment
	for _, c := range allComments {
		if m.isBotComment(c) {
			botComments = append(botComments, c)
		}
	}
	return botComments
}

// classifySummaryComment determines review status from the bot's summary comment.
// CodeRabbit posts "No actionable comments were generated" when the PR is clean.
func (m *ReviewMonitor) classifySummaryComment(body string, inlineComments []*github.PRReviewComment) ReviewStatus {
	bodyLower := strings.ToLower(body)

	// If there are inline comments, it's requesting changes regardless of summary
	if len(inlineComments) > 0 {
		return ReviewChangesRequested
	}

	// Check for explicit "clean" signals from CodeRabbit
	cleanSignals := []string{
		"no actionable comments",
		"no issues found",
		"lgtm",
		"looks good",
	}
	for _, signal := range cleanSignals {
		if strings.Contains(bodyLower, signal) {
			return ReviewApproved
		}
	}

	// Has a summary comment but no inline comments — treat as approved
	// (CodeRabbit always posts a summary; if no inline comments, it's clean)
	return ReviewApproved
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

	// Include CodeRabbit summary comment if present
	for _, c := range feedback.IssueComments {
		if c.Body != "" {
			b.WriteString("## Bot Review Summary\n")
			b.WriteString(c.Body)
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
