package main

import (
	"context"
	"log/slog"

	"github.com/alekspetrov/pilot/internal/adapters"
	"github.com/alekspetrov/pilot/internal/adapters/jira"
	"github.com/alekspetrov/pilot/internal/autopilot"
	"github.com/alekspetrov/pilot/internal/config"
	"github.com/alekspetrov/pilot/internal/logging"
)

func jiraPollerRegistration() PollerRegistration {
	return PollerRegistration{
		Name: "jira",
		Enabled: func(cfg *config.Config) bool {
			return cfg.Adapters.Jira != nil && cfg.Adapters.Jira.Enabled &&
				cfg.Adapters.Jira.Polling != nil && cfg.Adapters.Jira.Polling.Enabled
		},
		CreateAndStart: func(ctx context.Context, deps *PollerDeps) {
			// GH-1838: Jira adapter via common adapter interface + registry
			jiraAdapter := jira.NewAdapter(deps.Cfg.Adapters.Jira)
			adapters.Register(jiraAdapter)

			pollerDeps := adapters.PollerDeps{
				MaxConcurrent: deps.Cfg.Orchestrator.MaxConcurrent,
			}
			if deps.AutopilotStateStore != nil {
				pollerDeps.ProcessedStore = deps.AutopilotStateStore
			}

			// Wire Jira post-merge callback: transition ticket to Done when PR is merged
			if deps.AutopilotController != nil {
				jiraClient := jiraAdapter.Client()
				jiraCfg := deps.Cfg.Adapters.Jira
				deps.AutopilotController.SetOnMergedCallback(func(cbCtx context.Context, prState *autopilot.PRState) {
					if prState.SourceAdapter != "jira" || prState.SourceIssueKey == "" {
						return
					}
					log := logging.WithComponent("jira")
					log.Info("Transitioning Jira issue to Done after merge",
						slog.String("issue", prState.SourceIssueKey),
						slog.Int("pr", prState.PRNumber),
					)
					if jiraCfg.Transitions.Done != "" {
						if err := jiraClient.TransitionIssue(cbCtx, prState.SourceIssueKey, jiraCfg.Transitions.Done); err != nil {
							log.Warn("failed to transition Jira issue to Done (explicit ID)",
								slog.String("issue", prState.SourceIssueKey),
								slog.Any("error", err),
							)
						}
					} else {
						if err := jiraClient.TransitionIssueTo(cbCtx, prState.SourceIssueKey, "Done"); err != nil {
							log.Warn("failed to transition Jira issue to Done (name lookup)",
								slog.String("issue", prState.SourceIssueKey),
								slog.Any("error", err),
							)
						}
					}
				})
			}

			jiraPoller := jiraAdapter.CreatePoller(pollerDeps, func(issueCtx context.Context, issue *jira.Issue) (*jira.IssueResult, error) {
				result, err := handleJiraIssueWithResult(issueCtx, deps.Cfg, jiraAdapter.Client(), issue, deps.ProjectPath, deps.Dispatcher, deps.Runner, deps.Monitor, deps.Program, deps.AlertsEngine, deps.Enforcer)

				// Wire PR to autopilot with Jira source info for post-merge transition
				if result != nil && result.PRNumber > 0 && deps.AutopilotController != nil {
					logging.WithComponent("jira").Info("Wiring PR to autopilot",
						slog.String("issue", issue.Key),
						slog.Int("pr", result.PRNumber),
						slog.String("branch", result.BranchName),
						slog.String("sha", result.HeadSHA),
					)
					deps.AutopilotController.OnPRCreatedWithSource(result.PRNumber, result.PRURL, 0, result.HeadSHA, result.BranchName, "", "jira", issue.Key)
				} else if result != nil && deps.AutopilotController == nil {
					logging.WithComponent("jira").Warn("Autopilot controller not available, PR not tracked",
						slog.String("issue", issue.Key),
						slog.Int("pr", result.PRNumber),
					)
				}

				return result, err
			})

			logging.WithComponent("start").Info("Jira polling enabled",
				slog.String("base_url", deps.Cfg.Adapters.Jira.BaseURL),
				slog.String("project", deps.Cfg.Adapters.Jira.ProjectKey),
				slog.String("adapter", jiraAdapter.Name()),
			)
			go func(p *jira.Poller) {
				if err := p.Start(ctx); err != nil {
					logging.WithComponent("jira").Error("Jira poller failed",
						slog.Any("error", err),
					)
				}
			}(jiraPoller)
		},
	}
}
