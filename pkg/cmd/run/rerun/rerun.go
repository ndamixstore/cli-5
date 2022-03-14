package rerun

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/pkg/cmd/run/shared"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/spf13/cobra"
)

type RerunOptions struct {
	HttpClient func() (*http.Client, error)
	IO         *iostreams.IOStreams
	BaseRepo   func() (ghrepo.Interface, error)

	RunID      string
	OnlyFailed bool
	JobID      string

	Prompt bool
}

func NewCmdRerun(f *cmdutil.Factory, runF func(*RerunOptions) error) *cobra.Command {
	opts := &RerunOptions{
		IO:         f.IOStreams,
		HttpClient: f.HttpClient,
	}

	cmd := &cobra.Command{
		Use:   "rerun [<run-id>]",
		Short: "Rerun a failed run",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// support `-R, --repo` override
			opts.BaseRepo = f.BaseRepo

			if len(args) == 0 && opts.JobID == "" {
				if !opts.IO.CanPrompt() {
					return cmdutil.FlagErrorf("run or job ID required when not running interactively")
				} else {
					opts.Prompt = true
				}
			} else if len(args) > 0 {
				opts.RunID = args[0]
			}

			if opts.RunID != "" && opts.JobID != "" {
				opts.RunID = ""
				if opts.IO.CanPrompt() {
					cs := opts.IO.ColorScheme()
					fmt.Fprintf(opts.IO.ErrOut, "%s both run and job IDs specified; ignoring run ID\n", cs.WarningIcon())
				}
			}

			if runF != nil {
				return runF(opts)
			}
			return runRerun(opts)
		},
	}

	cmd.Flags().BoolVar(&opts.OnlyFailed, "failed", false, "Rerun only failed jobs")
	cmd.Flags().StringVarP(&opts.JobID, "job", "j", "", "Rerun a specific job from a run, including dependencies")

	return cmd
}

func runRerun(opts *RerunOptions) error {
	c, err := opts.HttpClient()
	if err != nil {
		return fmt.Errorf("failed to create http client: %w", err)
	}
	client := api.NewClientFromHTTP(c)

	repo, err := opts.BaseRepo()
	if err != nil {
		return fmt.Errorf("failed to determine base repo: %w", err)
	}

	cs := opts.IO.ColorScheme()

	runID := opts.RunID
	jobID := opts.JobID
	var selectedJob *shared.Job

	if jobID != "" {
		opts.IO.StartProgressIndicator()
		selectedJob, err = shared.GetJob(client, repo, jobID)
		opts.IO.StopProgressIndicator()
		if err != nil {
			return fmt.Errorf("failed to get job: %w", err)
		}
		runID = fmt.Sprintf("%d", selectedJob.RunID)
	}

	if opts.Prompt {
		runs, err := shared.GetRunsWithFilter(client, repo, nil, 10, func(run shared.Run) bool {
			if run.Status != shared.Completed {
				return false
			}
			// TODO StartupFailure indiciates a bad yaml file; such runs can never be
			// rerun. But hiding them from the prompt might confuse people?
			return run.Conclusion != shared.Success && run.Conclusion != shared.StartupFailure
		})
		if err != nil {
			return fmt.Errorf("failed to get runs: %w", err)
		}
		if len(runs) == 0 {
			return errors.New("no recent runs have failed; please specify a specific run ID")
		}
		runID, err = shared.PromptForRun(cs, runs)
		if err != nil {
			return err
		}
	}

	if opts.JobID != "" {
		err = rerunJob(client, repo, selectedJob)
		if err != nil {
			return err
		}
		if opts.IO.IsStdoutTTY() {
			fmt.Fprintf(opts.IO.Out, "%s Requested rerun of job %s on run %s\n",
				cs.SuccessIcon(),
				cs.Cyanf("%d", selectedJob.ID),
				cs.Cyanf("%d", selectedJob.RunID))
		}
	} else {
		opts.IO.StartProgressIndicator()
		run, err := shared.GetRun(client, repo, runID)
		opts.IO.StopProgressIndicator()
		if err != nil {
			return fmt.Errorf("failed to get run: %w", err)
		}

		err = rerunRun(client, repo, run, opts.OnlyFailed)
		if err != nil {
			return err
		}
		if opts.IO.IsStdoutTTY() {
			onlyFailedMsg := ""
			if opts.OnlyFailed {
				onlyFailedMsg = "(failed jobs) "
			}
			fmt.Fprintf(opts.IO.Out, "%s Requested rerun %sof run %s\n",
				cs.SuccessIcon(),
				onlyFailedMsg,
				cs.Cyanf("%d", run.ID))
		}
	}

	return nil
}

func rerunRun(client *api.Client, repo ghrepo.Interface, run *shared.Run, onlyFailed bool) error {
	runVerb := "rerun"
	if onlyFailed {
		runVerb = "rerun-failed-jobs"
	}

	path := fmt.Sprintf("repos/%s/actions/runs/%d/%s", ghrepo.FullName(repo), run.ID, runVerb)

	err := client.REST(repo.RepoHost(), "POST", path, nil, nil)
	if err != nil {
		var httpError api.HTTPError
		if errors.As(err, &httpError) && httpError.StatusCode == 403 {
			return fmt.Errorf("run %d cannot be rerun; its workflow file may be broken", run.ID)
		}
		return fmt.Errorf("failed to rerun: %w", err)
	}
	return nil
}

func rerunJob(client *api.Client, repo ghrepo.Interface, job *shared.Job) error {
	path := fmt.Sprintf("repos/%s/actions/jobs/%d/rerun", ghrepo.FullName(repo), job.ID)

	err := client.REST(repo.RepoHost(), "POST", path, nil, nil)
	if err != nil {
		var httpError api.HTTPError
		if errors.As(err, &httpError) && httpError.StatusCode == 403 {
			return fmt.Errorf("job %d cannot be rerun", job.ID)
		}
		return fmt.Errorf("failed to rerun: %w", err)
	}
	return nil
}
