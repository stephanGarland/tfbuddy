package hooks

import (
	"errors"
	"fmt"

	gogithub "github.com/google/go-github/v49/github"
	"github.com/rs/zerolog/log"
	"github.com/zapier/tfbuddy/pkg/allow_list"
	"github.com/zapier/tfbuddy/pkg/comment_actions"
	"github.com/zapier/tfbuddy/pkg/github"
	"github.com/zapier/tfbuddy/pkg/tfc_trigger"
	"github.com/zapier/tfbuddy/pkg/utils"
)

func (h *GithubHooksHandler) processIssueCommentEvent(msg *GithubIssueCommentEventMsg) error {
	var commentErr error
	defer func() {
		if r := recover(); r != nil {
			log.Error().Msgf("Unrecoverable error in issue comment event processing %v", r)
			commentErr = nil
		}
	}()
	commentErr = h.processIssueComment(msg)
	return utils.EmitPermanentError(commentErr, func(err error) {
		log.Error().Msgf("got a permanent error attempting to process comment event: %s", err.Error())
	})
}

func (h *GithubHooksHandler) processIssueComment(msg *GithubIssueCommentEventMsg) error {
	if msg == nil || msg.payload == nil {
		return errors.New("msg is nil")
	}
	event := msg.payload

	// Check if fullName is allowed
	log.Debug().Str("repo", *event.Repo.FullName).Msg("processIssueCommentEvent")
	fullName := event.Repo.FullName
	if !allow_list.IsGithubRepoAllowed(*fullName) {
		return nil
	}

	// Parse comment
	opts, err := comment_actions.ParseCommentCommand(*event.Comment.Body)
	if err != nil {
		if err == comment_actions.ErrOtherTFTool {
			h.postPullRequestComment(event, "Use 'tfc' to interact with TFBuddy")
		}
		if err == comment_actions.ErrNotTFCCommand || err == comment_actions.ErrOtherTFTool {
			githubWebHookIgnored.WithLabelValues(
				"issue_comment_created",
				*fullName,
				"not-tfc-command",
			).Inc()
			return nil
		}
		return err
	}

	pr, err := h.vcs.GetMergeRequest(*event.Issue.Number, event.GetRepo().GetFullName())
	if err != nil {
		log.Error().Err(err).Msg("could not process GitHub IssueCommentEvent")
		return err
	}
	pullReq := pr.(*github.GithubPR)

	opts.TriggerOpts.Branch = pr.GetSourceBranch()
	opts.TriggerOpts.CommitSHA = pullReq.GetBase().GetSHA()
	opts.TriggerOpts.ProjectNameWithNamespace = event.GetRepo().GetFullName()
	opts.TriggerOpts.MergeRequestIID = *event.Issue.Number
	opts.TriggerOpts.TriggerSource = tfc_trigger.CommentTrigger
	opts.TriggerOpts.VcsProvider = "github"

	cfg, err := tfc_trigger.NewTFCTriggerConfig(opts.TriggerOpts)
	if err != nil {
		log.Error().Err(err).Msg("could not create TFCTriggerConfig")
		return err
	}

	trigger := h.triggerCreation(h.vcs, h.tfc, h.runstream, cfg)

	//// TODO: support additional commands and arguments (e.g. destroy, refresh, lock, unlock)
	//// TODO: this should be refactored and be agnostic to the VCS type
	switch opts.Args.Command {
	case "apply":
		log.Info().Msg("Got TFC apply command")
		if !pullReq.IsApproved() {
			h.postPullRequestComment(event, ":no_entry: Apply failed. Pull Request requires approval.")
			return nil
		}

		if pullReq.HasConflicts() {
			h.postPullRequestComment(event, ":no_entry: Apply failed. Pull Request has conflicts that need to be resolved.")
			return nil
		}
	case "lock":
		log.Info().Msg("Got TFC lock command")
	case "plan":
		log.Info().Msg("Got TFC plan command")
	case "unlock":
		log.Info().Msg("Got TFC unlock command")
	default:
		return fmt.Errorf("could not parse command")
	}
	executedWorkspaces, tfError := trigger.TriggerTFCEvents()
	if tfError == nil && len(executedWorkspaces.Errored) > 0 {
		for _, failedWS := range executedWorkspaces.Errored {
			h.postPullRequestComment(event, fmt.Sprintf(":no_entry: %s could not be run because: %s", failedWS.Name, failedWS.Error))
		}
		return nil
	}
	return tfError
}

func (h *GithubHooksHandler) postPullRequestComment(event *gogithub.IssueCommentEvent, body string) error {
	log.Debug().Msg("postPullRequestComment")

	prID := event.GetIssue().GetNumber()
	log.Debug().Str("repo", event.GetRepo().GetFullName()).Int("PR", prID).Msg("postPullRequestComment")
	return h.vcs.CreateMergeRequestComment(prID, event.GetRepo().GetFullName(), body)
}
