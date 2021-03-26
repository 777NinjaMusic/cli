package run

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/cli/cli/api"
	"github.com/cli/cli/internal/ghrepo"
	"github.com/cli/cli/pkg/cmd/workflow/shared"
	"github.com/cli/cli/pkg/cmdutil"
	"github.com/cli/cli/pkg/iostreams"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

type RunOptions struct {
	HttpClient func() (*http.Client, error)
	IO         *iostreams.IOStreams
	BaseRepo   func() (ghrepo.Interface, error)

	Selector string
	Ref      string

	InputArgs []string
	JSON      string

	Prompt bool
}

func NewCmdRun(f *cmdutil.Factory, runF func(*RunOptions) error) *cobra.Command {
	opts := &RunOptions{
		IO:         f.IOStreams,
		HttpClient: f.HttpClient,
	}

	cmd := &cobra.Command{
		Use:   "run [<workflow ID> | <workflow name>]",
		Short: "Create a dispatch event for a workflow, starting a run",
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() == 0 && len(args[1:]) > 0 {
				return cmdutil.FlagError{Err: fmt.Errorf("workflow argument required when passing input flags")}
			}
			return nil
		},
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// support `-R, --repo` override
			opts.BaseRepo = f.BaseRepo

			if len(args) > 0 {
				opts.Selector = args[0]
				opts.InputArgs = args[1:]
			} else if !opts.IO.CanPrompt() {
				return &cmdutil.FlagError{Err: errors.New("workflow ID or name required when not running interactively")}
			} else {
				opts.Prompt = true
			}

			if !opts.IO.IsStdinTTY() {
				jsonIn, err := ioutil.ReadAll(opts.IO.In)
				if err != nil {
					return errors.New("failed to read from STDIN")
				}
				opts.JSON = string(jsonIn)
			}

			if opts.Selector == "" {
				if opts.JSON != "" {
					return &cmdutil.FlagError{Err: errors.New("workflow argument required when passing JSON")}
				}
			} else {
				if opts.JSON != "" && len(opts.InputArgs) > 0 {
					return &cmdutil.FlagError{Err: errors.New("only one of JSON or input arguments can be passed at a time")}
				}
			}

			if runF != nil {
				return runF(opts)
			}
			return runRun(opts)
		},
	}
	cmd.Flags().StringVarP(&opts.Ref, "ref", "r", "", "The branch or tag name which contains the version of the workflow file you'd like to run")
	cmd.Flags().StringVar(&opts.JSON, "json", "", "TODO")

	return cmd
}

func runRun(opts *RunOptions) error {
	c, err := opts.HttpClient()
	if err != nil {
		return fmt.Errorf("could not build http client: %w", err)
	}
	client := api.NewClientFromHTTP(c)

	repo, err := opts.BaseRepo()
	if err != nil {
		return fmt.Errorf("could not determine base repo: %w", err)
	}

	states := []shared.WorkflowState{shared.Active}
	workflow, err := shared.ResolveWorkflow(
		opts.IO, client, repo, opts.Prompt, opts.Selector, states)
	if err != nil {
		var fae shared.FilteredAllError
		if errors.As(err, &fae) {
			return errors.New("no workflows are enabled on this repository")
		}
		return err
	}

	// TODO  once end-to-end is working, circle back and see if running a local workflow remotely is feasible by doing git stuff automagically in a throwaway branch.
	ref := opts.Ref

	if ref == "" {
		ref, err = api.RepoDefaultBranch(client, repo)
		if err != nil {
			return fmt.Errorf("unable to determine default branch for %s: %w", ghrepo.FullName(repo), err)
		}
	}

	yamlContent, err := getWorkflowContent(client, repo, workflow, ref)
	if err != nil {
		return fmt.Errorf("unable to fetch workflow file content: %w", err)
	}

	inputs, err := findInputs(yamlContent)
	if err != nil {
		return err
	}

	providedInputs := map[string]string{}

	// TODO is opts.Prompt doing too much here?
	if opts.Prompt {
		// TODO survey version
		return nil
	} else {
		if opts.JSON != "" {
			err := json.Unmarshal([]byte(opts.JSON), providedInputs)
			if err != nil {
				return fmt.Errorf("could not parse provided JSON: %w", err)
			}
		}

		if len(opts.InputArgs) > 0 {
			fs := pflag.FlagSet{}
			//var test string
			for inputName, input := range inputs {
				fs.String(inputName, input.Default, input.Description)
			}
			err = fs.Parse(opts.InputArgs)
			if err != nil {
				return fmt.Errorf("could not parse input args: %w", err)
			}
			for inputName, input := range inputs {
				// TODO error handling
				providedValue, _ := fs.GetString(inputName)

				if input.Required && providedValue == "" {
					return fmt.Errorf("missing required input '%s'", inputName)
				}

				providedInputs[inputName] = providedValue
			}
		}
	}

	fmt.Printf("DBG %#v\n", providedInputs)

	// TODO generate survey prompts for the inputs
	// TODO validate whatever input we got
	// TODO create the dispatch event

	return nil
}

type WorkflowInput struct {
	// TODO i'd put Name in here but that's not how the yaml is structured. decide if things should be inconsistent or not.
	Required    bool
	Default     string
	Description string
}

func findInputs(yamlContent []byte) (map[string]WorkflowInput, error) {
	var rootNode yaml.Node
	err := yaml.Unmarshal(yamlContent, &rootNode)
	if err != nil {
		return nil, fmt.Errorf("unable to parse workflow YAML: %w", err)
	}

	if len(rootNode.Content) != 1 {
		return nil, errors.New("invalid yaml file")
	}

	var onKeyNode *yaml.Node
	var dispatchKeyNode *yaml.Node
	var inputsKeyNode *yaml.Node
	var inputsMapNode *yaml.Node

	// TODO this is pretty hideous
	for _, node := range rootNode.Content[0].Content {
		if onKeyNode != nil {
			for _, node := range node.Content {
				if dispatchKeyNode != nil {
					for _, node := range node.Content {
						if inputsKeyNode != nil {
							inputsMapNode = node
							break
						}
						if node.Value == "inputs" {
							inputsKeyNode = node
						}
					}
					break
				}
				if node.Value == "workflow_dispatch" {
					dispatchKeyNode = node
				}
			}
			break
		}
		if strings.EqualFold(node.Value, "on") {
			onKeyNode = node
		}
	}

	if onKeyNode == nil {
		return nil, errors.New("invalid workflow: no 'on' key")
	}

	if dispatchKeyNode == nil {
		return nil, errors.New("unable to manually run a workflow without a workflow_dispatch event")
	}

	out := map[string]WorkflowInput{}

	if inputsKeyNode == nil || inputsMapNode == nil {
		return out, nil
	}

	err = inputsMapNode.Decode(&out)
	if err != nil {
		return nil, fmt.Errorf("could not decode workflow inputs: %w", err)
	}

	return out, nil
}

func getWorkflowContent(client *api.Client, repo ghrepo.Interface, workflow *shared.Workflow, ref string) ([]byte, error) {
	path := fmt.Sprintf("repos/%s/contents/%s?ref=%s", ghrepo.FullName(repo), workflow.Path, url.QueryEscape(ref))

	type Result struct {
		Content string
	}

	var result Result
	err := client.REST(repo.RepoHost(), "GET", path, nil, &result)
	if err != nil {
		return nil, err
	}

	decoded, err := base64.StdEncoding.DecodeString(result.Content)
	if err != nil {
		return nil, fmt.Errorf("failed to decode workflow file: %w", err)
	}

	return decoded, nil
}
