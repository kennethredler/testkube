package testworkflows

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubeshop/testkube/cmd/kubectl-testkube/commands/common"
	"github.com/kubeshop/testkube/cmd/kubectl-testkube/commands/common/render"
	"github.com/kubeshop/testkube/cmd/kubectl-testkube/commands/testworkflows/renderer"
	"github.com/kubeshop/testkube/cmd/testworkflow-init/data"
	apiclientv1 "github.com/kubeshop/testkube/pkg/api/v1/client"
	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/ui"
)

const (
	LogTimestampLength = 30 // time.RFC3339Nano without 00:00 timezone
)

var (
	NL = []byte("\n")
)

func NewRunTestWorkflowCmd() *cobra.Command {
	var (
		executionName string
		config        map[string]string
		watchEnabled  bool
	)

	cmd := &cobra.Command{
		Use:     "testworkflow [name]",
		Aliases: []string{"testworkflows", "tw"},
		Args:    cobra.ExactArgs(1),
		Short:   "Starts test workflow execution",

		Run: func(cmd *cobra.Command, args []string) {
			if common.IsBothEnabledAndDisabledSet(cmd) {
				ui.Failf("both --enable-webhooks and --disable-webhooks flags are set, please use only one")
			}

			outputFlag := cmd.Flag("output")
			outputType := render.OutputPretty
			if outputFlag != nil {
				outputType = render.OutputType(outputFlag.Value.String())
			}

			outputPretty := outputType == render.OutputPretty
			namespace := cmd.Flag("namespace").Value.String()
			client, _, err := common.GetClient(cmd)
			ui.ExitOnError("getting client", err)

			var disableWebhooks bool
			if cmd.Flag("enable-webhooks").Changed {
				disableWebhooks = false
			}
			if cmd.Flag("disable-webhooks").Changed {
				disableWebhooks = true
			}

			name := args[0]
			execution, err := client.ExecuteTestWorkflow(name, testkube.TestWorkflowExecutionRequest{
				Name:            executionName,
				Config:          config,
				DisableWebhooks: disableWebhooks,
			})
			ui.ExitOnError("execute test workflow "+name+" from namespace "+namespace, err)
			err = renderer.PrintTestWorkflowExecution(cmd, os.Stdout, execution)
			ui.ExitOnError("render test workflow execution", err)

			var exitCode = 0
			if outputPretty {
				ui.NL()
				if watchEnabled {
					exitCode = uiWatch(execution, client)
					ui.NL()
				} else {
					uiShellWatchExecution(execution.Id)
				}

				uiShellGetExecution(execution.Id)
			}

			os.Exit(exitCode)
		},
	}

	cmd.Flags().StringVarP(&executionName, "name", "n", "", "execution name, if empty will be autogenerated")
	cmd.Flags().StringToStringVarP(&config, "config", "", map[string]string{}, "configuration variables in a form of name1=val1 passed to executor")
	cmd.Flags().BoolVarP(&watchEnabled, "watch", "f", false, "watch for changes after start")
	cmd.Flags().Bool("disable-webhooks", false, "disable webhooks for this execution")
	cmd.Flags().Bool("enable-webhooks", false, "enable webhooks for this execution")

	return cmd
}

func uiWatch(execution testkube.TestWorkflowExecution, client apiclientv1.Client) int {
	result, err := watchTestWorkflowLogs(execution.Id, execution.Signature, client)
	ui.ExitOnError("reading test workflow execution logs", err)

	// Apply the result in the execution
	execution.Result = result
	if result.IsFinished() {
		execution.StatusAt = result.FinishedAt
	}

	// Display message depending on the result
	switch {
	case result.Initialization.ErrorMessage != "":
		ui.Warn("test workflow execution failed:\n")
		ui.Errf(result.Initialization.ErrorMessage)
		return 1
	case result.IsFailed():
		ui.Warn("test workflow execution failed")
		return 1
	case result.IsAborted():
		ui.Warn("test workflow execution aborted")
		return 1
	case result.IsPassed():
		ui.Success("test workflow execution completed with success in " + result.FinishedAt.Sub(result.QueuedAt).String())
	}
	return 0
}

func uiShellGetExecution(id string) {
	ui.ShellCommand(
		"Use following command to get test workflow execution details",
		"kubectl testkube get twe "+id,
	)
}

func uiShellWatchExecution(id string) {
	ui.ShellCommand(
		"Watch test workflow execution until complete",
		"kubectl testkube watch twe "+id,
	)
}

func flattenSignatures(sig []testkube.TestWorkflowSignature) []testkube.TestWorkflowSignature {
	res := make([]testkube.TestWorkflowSignature, 0)
	for _, s := range sig {
		if len(s.Children) == 0 {
			res = append(res, s)
		} else {
			res = append(res, flattenSignatures(s.Children)...)
		}
	}
	return res
}

func printSingleResultDifference(r1 testkube.TestWorkflowStepResult, r2 testkube.TestWorkflowStepResult, signature testkube.TestWorkflowSignature, index int, steps int) bool {
	r1Status := testkube.QUEUED_TestWorkflowStepStatus
	r2Status := testkube.QUEUED_TestWorkflowStepStatus
	if r1.Status != nil {
		r1Status = *r1.Status
	}
	if r2.Status != nil {
		r2Status = *r2.Status
	}
	if r1Status == r2Status {
		return false
	}
	name := signature.Category
	if signature.Name != "" {
		name = signature.Name
	}
	took := r2.FinishedAt.Sub(r2.QueuedAt).Round(time.Millisecond)

	printStatus(signature, r2Status, took, index, steps, name)
	return true
}

func printResultDifference(res1 *testkube.TestWorkflowResult, res2 *testkube.TestWorkflowResult, steps []testkube.TestWorkflowSignature) bool {
	if res1 == nil || res2 == nil {
		return false
	}
	changed := printSingleResultDifference(*res1.Initialization, *res2.Initialization, testkube.TestWorkflowSignature{Name: "Initializing"}, -1, len(steps))
	for i, s := range steps {
		changed = changed || printSingleResultDifference(res1.Steps[s.Ref], res2.Steps[s.Ref], s, i, len(steps))
	}

	return changed
}

func getTimestampLength(line string) int {
	// 29th character will be either '+' for +00:00 timestamp,
	// or 'Z' for UTC timestamp (without 00:00 section).
	if len(line) >= 29 && line[29] == '+' {
		return len(time.RFC3339Nano)
	}
	return LogTimestampLength
}

func watchTestWorkflowLogs(id string, signature []testkube.TestWorkflowSignature, client apiclientv1.Client) (*testkube.TestWorkflowResult, error) {
	ui.Info("Getting logs from test workflow job", id)

	notifications, err := client.GetTestWorkflowExecutionNotifications(id)
	ui.ExitOnError("getting logs from executor", err)

	steps := flattenSignatures(signature)

	var result *testkube.TestWorkflowResult
	var isLineBeginning = true
	for l := range notifications {
		if l.Output != nil {
			continue
		}
		if l.Result != nil {
			if printResultDifference(result, l.Result, steps) {
				isLineBeginning = true
			}
			result = l.Result
			continue
		}

		printStructuredLogLines(l.Log, &isLineBeginning)
	}

	ui.NL()

	return result, err
}

func printStatusHeader(i, n int, name string) {
	if i == -1 {
		fmt.Println("\n" + ui.LightCyan(fmt.Sprintf("• %s", name)))
	} else {
		fmt.Println("\n" + ui.LightCyan(fmt.Sprintf("• (%d/%d) %s", i+1, n, name)))
	}
}

func printStatus(s testkube.TestWorkflowSignature, rStatus testkube.TestWorkflowStepStatus, took time.Duration,
	i, n int, name string) {
	switch rStatus {
	case testkube.RUNNING_TestWorkflowStepStatus:
		printStatusHeader(i, n, name)
	case testkube.SKIPPED_TestWorkflowStepStatus:
		fmt.Println(ui.LightGray("• skipped"))
	case testkube.PASSED_TestWorkflowStepStatus:
		fmt.Println("\n" + ui.Green(fmt.Sprintf("• passed in %s", took)))
	case testkube.ABORTED_TestWorkflowStepStatus:
		fmt.Println("\n" + ui.Red("• aborted"))
	default:
		if s.Optional {
			fmt.Println("\n" + ui.Yellow(fmt.Sprintf("• %s in %s (ignored)", string(rStatus), took)))
		} else {
			fmt.Println("\n" + ui.Red(fmt.Sprintf("• %s in %s", string(rStatus), took)))
		}
	}
}

func printStructuredLogLines(logs string, isLineBeginning *bool) {
	// Strip timestamp + space for all new lines in the log
	for len(logs) > 0 {
		if *isLineBeginning {
			logs = logs[getTimestampLength(logs)+1:]
			*isLineBeginning = false
		}

		newLineIndex := strings.Index(logs, "\n")
		if newLineIndex == -1 {
			fmt.Print(logs)
			break
		} else {
			fmt.Print(logs[0 : newLineIndex+1])
			logs = logs[newLineIndex+1:]
			*isLineBeginning = true
		}
	}
}

func printRawLogLines(logs []byte, steps []testkube.TestWorkflowSignature, results map[string]testkube.TestWorkflowStepResult) {
	currentRef := ""
	i := -1
	printStatusHeader(-1, len(steps), "Initializing")
	// Strip timestamp + space for all new lines in the log
	for len(logs) > 0 {
		newLineIndex := bytes.Index(logs, NL)
		var line string
		if newLineIndex == -1 {
			line = string(logs)
			logs = nil
		} else {
			line = string(logs[:newLineIndex])
			logs = logs[newLineIndex+1:]
		}

		if len(line) >= LogTimestampLength-1 {
			line = line[getTimestampLength(line)+1:]
		}

		start := data.StartHintRe.FindStringSubmatch(line)
		if len(start) == 0 {
			line += "\x07"
			fmt.Println(line)
			continue
		}

		nextRef := start[1]

		for i == -1 || steps[i].Ref != nextRef {
			if ps, ok := results[currentRef]; ok && ps.Status != nil {
				took := ps.FinishedAt.Sub(ps.QueuedAt).Round(time.Millisecond)
				printStatus(steps[i], *ps.Status, took, i, len(steps), steps[i].Label())
			}

			i++
			currentRef = steps[i].Ref
			printStatusHeader(i, len(steps), steps[i].Label())
		}
	}

	for _, step := range steps[i:] {
		if ps, ok := results[currentRef]; ok && ps.Status != nil {
			took := ps.FinishedAt.Sub(ps.QueuedAt).Round(time.Millisecond)
			printStatus(step, *ps.Status, took, i, len(steps), steps[i].Label())
		}

		i++
		currentRef = step.Ref
		if i < len(steps) {
			printStatusHeader(i, len(steps), steps[i].Label())
		}
	}
}
