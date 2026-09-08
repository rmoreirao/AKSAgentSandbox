package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type upFlags struct {
	repository     string
	empty          bool
	template       string
	profile        string
	name           string
	ref            string
	idleTimeout    time.Duration
	resumeExisting bool
	createNew      bool
	noAttach       bool
}

func (a *application) upCommand() *cobra.Command {
	flags := upFlags{}
	defaultTemplate := strings.TrimSpace(os.Getenv("DEVSANDBOX_DEFAULT_TEMPLATE"))
	command := &cobra.Command{
		Use:   "up",
		Short: "Create or resume a sandbox and wait until it is ready",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return a.runUp(command, flags)
		},
	}
	command.Flags().StringVar(&flags.repository, "repo", "", "GitHub OWNER/REPOSITORY or repository URL")
	command.Flags().BoolVar(&flags.empty, "empty", false, "create an empty workspace")
	command.Flags().StringVar(&flags.template, "template", defaultTemplate,
		"template name (or DEVSANDBOX_DEFAULT_TEMPLATE)")
	command.Flags().StringVar(&flags.profile, "profile", "", "resource profile: small, medium, or large")
	command.Flags().StringVar(&flags.name, "name", "", "DNS-safe sandbox name")
	command.Flags().StringVar(&flags.ref, "ref", "", "repository branch, tag, or commit")
	command.Flags().DurationVar(&flags.idleTimeout, "idle-timeout", 0, "idle timeout duration")
	command.Flags().BoolVar(&flags.resumeExisting, "resume-existing", false, "resume an equivalent stopped sandbox")
	command.Flags().BoolVar(&flags.createNew, "new", false, "always create a new sandbox")
	command.Flags().BoolVar(&flags.noAttach, "no-attach", false, "return when the sandbox is ready")
	command.MarkFlagsMutuallyExclusive("repo", "empty")
	command.MarkFlagsMutuallyExclusive("resume-existing", "new")
	return command
}

func (a *application) runUp(command *cobra.Command, flags upFlags) error {
	ctx := command.Context()
	client, err := a.client(ctx, true)
	if err != nil {
		return err
	}
	templates, err := client.Templates(ctx)
	if err != nil {
		return err
	}
	templateNames := make([]string, 0, len(templates))
	templateByName := make(map[string]Template, len(templates))
	for _, item := range templates {
		templateNames = append(templateNames, item.Name)
		templateByName[item.Name] = item
	}
	sort.Strings(templateNames)
	if flags.template == "" {
		flags.template, err = a.prompt.Select("A template is required; choose a template", templateNames)
		if err != nil {
			return err
		}
	}
	template, ok := templateByName[flags.template]
	if !ok {
		return cliError(ExitNotFound, "template_not_found",
			fmt.Sprintf("template %q was not found", flags.template), map[string]any{"choices": templateNames})
	}
	if flags.profile == "" {
		flags.profile = template.DefaultProfile
	}
	if flags.profile != "small" && flags.profile != "medium" && flags.profile != "large" {
		return invalid("--profile must be small, medium, or large")
	}
	if command.Flags().Changed("idle-timeout") &&
		(flags.idleTimeout <= 0 || flags.idleTimeout > 7200*time.Second) {
		return invalid("--idle-timeout must be greater than zero and no more than 2h")
	}
	if flags.idleTimeout/time.Second > math.MaxInt32 {
		return invalid("--idle-timeout is too large")
	}

	source, err := a.resolveUpSource(ctx, client, flags)
	if err != nil {
		return err
	}
	if flags.name == "" {
		flags.name, err = GenerateName(namePrefix(source, flags.template), a.options.Random)
		if err != nil {
			return cliError(ExitInternal, "name_generation_failed", err.Error(), nil)
		}
	}
	if !dnsNamePattern.MatchString(flags.name) {
		return invalid("--name must be a lowercase DNS label of at most 63 characters")
	}

	existing, err := client.Sandboxes(ctx, true)
	if err != nil {
		return err
	}
	equivalent := make([]Sandbox, 0)
	for _, sandbox := range existing {
		if equivalentSandbox(sandbox, source, flags.template, flags.profile) {
			equivalent = append(equivalent, sandbox)
		}
	}
	sort.Slice(equivalent, func(i, j int) bool { return equivalent[i].Name < equivalent[j].Name })
	if flags.resumeExisting {
		if len(equivalent) == 0 {
			return cliError(ExitNotFound, "sandbox_not_found", "no equivalent stopped sandbox was found", nil)
		}
		selected, selectErr := a.selectSandbox("Choose an equivalent stopped sandbox to resume", equivalent)
		if selectErr != nil {
			return selectErr
		}
		value, resumeErr := client.ResumeSandbox(ctx, selected.Name)
		if resumeErr != nil {
			return resumeErr
		}
		return a.finishUp(ctx, client, value, template.EntryAction, flags.noAttach)
	}
	if len(equivalent) > 0 && !flags.createNew {
		choices := make([]string, 0, len(equivalent)+1)
		for _, value := range equivalent {
			choices = append(choices, "resume "+value.Name)
		}
		choices = append(choices, "create new")
		choice, selectErr := a.prompt.Select("An equivalent stopped sandbox exists; resume it or create a new sandbox", choices)
		if selectErr != nil {
			return selectErr
		}
		if choice != "create new" {
			name := strings.TrimPrefix(choice, "resume ")
			value, resumeErr := client.ResumeSandbox(ctx, name)
			if resumeErr != nil {
				return resumeErr
			}
			return a.finishUp(ctx, client, value, template.EntryAction, flags.noAttach)
		}
	}
	value, err := client.CreateSandbox(ctx, CreateSandboxRequest{
		Name: flags.name, Source: source, Template: flags.template, Profile: flags.profile,
		IdleTimeoutSeconds: int32(flags.idleTimeout / time.Second), CreateNew: flags.createNew,
	})
	if err != nil {
		return err
	}
	return a.finishUp(ctx, client, value, template.EntryAction, flags.noAttach)
}

func (a *application) resolveUpSource(ctx context.Context, client *Client, flags upFlags) (SandboxSource, error) {
	if flags.empty {
		if flags.ref != "" {
			return SandboxSource{}, invalid("--ref cannot be used with --empty")
		}
		return SandboxSource{Type: "empty"}, nil
	}
	var target RepositoryTarget
	var err error
	if flags.repository != "" {
		target, err = ParseRepository(flags.repository)
	} else {
		target, err = a.options.Resolver.Current(ctx, a.prompt)
		if err != nil {
			var cliErr *Error
			if errors.As(err, &cliErr) && cliErr.Code == "repository_not_found" {
				return SandboxSource{}, cliError(ExitInvalid, "repository_required",
					"no current Git repository was found; use --repo or --empty",
					map[string]any{"choices": []string{"--repo OWNER/REPOSITORY", "--empty"}})
			}
		}
	}
	if err != nil {
		return SandboxSource{}, err
	}
	if flags.repository != "" {
		target, err = client.ResolveRepository(ctx, target.ID, flags.ref)
		if err != nil {
			return SandboxSource{}, err
		}
	} else {
		resolved, resolveErr := client.ResolveRepository(ctx, target.ID, target.CommitSHA)
		if resolveErr != nil {
			return SandboxSource{}, resolveErr
		}
		if !strings.EqualFold(resolved.CommitSHA, target.CommitSHA) {
			return SandboxSource{}, cliError(ExitInvalid, "repository_unpushed",
				"local HEAD is not available from the selected GitHub repository", nil)
		}
		resolved.Ref = target.Ref
		if target.AuthorName != "" {
			resolved.AuthorName = target.AuthorName
		}
		if target.AuthorEmail != "" {
			resolved.AuthorEmail = target.AuthorEmail
		}
		target = resolved
	}
	return SandboxSource{
		Type: "git", RepositoryID: target.ID, RepositoryURL: target.URL,
		RefName: target.Ref, CommitSHA: target.CommitSHA,
		LFS: true, Submodules: true, AuthorName: target.AuthorName, AuthorEmail: target.AuthorEmail,
		PrimaryOrg: target.PrimaryOrg, ReadOnly: target.ReadOnly,
	}, nil
}

func (a *application) finishUp(ctx context.Context, client *Client, value Sandbox, entryAction string, noAttach bool) error {
	ready, err := a.waitReady(ctx, client, value)
	if err != nil {
		return err
	}
	if a.json {
		if err := writeJSON(a.options.Out, ready); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(a.options.Out, "Sandbox %s is Running\n", ready.Name)
	}
	if noAttach {
		return nil
	}
	switch entryAction {
	case "vscode":
		target, urlErr := client.VSCodeURL(ctx, ready.Name)
		if urlErr != nil {
			return urlErr
		}
		if !a.json {
			_, _ = fmt.Fprintln(a.options.Out, "Opening the authenticated VS Code URL")
		}
		return a.options.OpenURL(target)
	case "copilot", "shell", "":
		action := entryAction
		if action == "" {
			action = "shell"
		}
		return a.options.Attach(ctx, client, ready.Name, action, a.options.In, a.options.Out, a.options.Err)
	default:
		return cliError(ExitInternal, "unknown_entry_action",
			fmt.Sprintf("template has unsupported entry action %q", entryAction), nil)
	}
}

func (a *application) waitReady(ctx context.Context, client *Client, value Sandbox) (Sandbox, error) {
	seenEvents := make(map[string]bool)
	for {
		switch value.Phase {
		case "Running":
			return value, nil
		case "Failed":
			return Sandbox{}, cliError(ExitProvisioning, "provisioning_failed",
				fmt.Sprintf("sandbox %s failed to provision", value.Name), map[string]any{"conditions": value.Conditions})
		case "Stopped", "Deleting":
			return Sandbox{}, cliError(ExitConflict, "invalid_state",
				fmt.Sprintf("sandbox %s entered state %s while waiting", value.Name, value.Phase), nil)
		}
		if !a.json {
			events, eventErr := client.Events(ctx, value.Name)
			if eventErr == nil {
				for _, event := range events {
					key := event.ID + "|" + event.OccurredAt.String() + "|" + event.Message
					if !seenEvents[key] {
						seenEvents[key] = true
						_, _ = fmt.Fprintf(a.options.Out, "%s: %s\n", event.Type, event.Message)
					}
				}
			}
		}
		if err := a.options.Sleep(ctx, a.options.PollInterval); err != nil {
			return Sandbox{}, err
		}
		var err error
		value, err = client.Sandbox(ctx, value.Name)
		if err != nil {
			return Sandbox{}, err
		}
	}
}

func (a *application) listCommand() *cobra.Command {
	var all bool
	command := &cobra.Command{
		Use:   "list",
		Short: "List your sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			items, err := client.Sandboxes(command.Context(), all)
			if err != nil {
				return err
			}
			filtered := make([]Sandbox, 0, len(items))
			for _, item := range items {
				if all || visibleActivePhase(item.Phase) {
					filtered = append(filtered, item)
				}
			}
			sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })
			if a.json {
				return writeJSON(a.options.Out, map[string]any{"items": filtered})
			}
			writeSandboxTable(a.options.Out, filtered, time.Now())
			return nil
		},
	}
	command.Flags().BoolVar(&all, "all", false, "include stopped and failed sandboxes")
	return command
}

func visibleActivePhase(phase string) bool {
	switch phase {
	case "Provisioning", "Initializing", "Running", "Resuming", "Stopping":
		return true
	default:
		return false
	}
}

func writeSandboxTable(out io.Writer, items []Sandbox, now time.Time) {
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "NAME\tSTATE\tTEMPLATE\tPROFILE\tSOURCE\tAGE\tLAST ACTIVITY\tDEADLINE")
	for _, item := range items {
		source := "empty"
		if item.Source.Type == "git" {
			source = item.Source.RepositoryID
			if item.Source.RefName != "" {
				source += "@" + item.Source.RefName
			}
		}
		last := "-"
		if item.LastActivityTime != nil {
			last = relativeDuration(now.Sub(*item.LastActivityTime))
		}
		deadline := "-"
		if item.Phase == "Running" && item.IdleDeadline != nil {
			deadline = item.IdleDeadline.Format(time.RFC3339)
		} else if item.RetentionDeadline != nil {
			deadline = item.RetentionDeadline.Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s@%s\t%s\t%s\t%s\t%s\t%s\n",
			item.Name, item.Phase, item.Template.Name, item.Template.Version, item.Profile.Name, source,
			relativeDuration(now.Sub(item.CreatedAt)), last, deadline)
	}
	_ = writer.Flush()
}

func relativeDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	if duration < time.Minute {
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	}
	if duration < time.Hour {
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	}
	if duration < 24*time.Hour {
		return fmt.Sprintf("%dh", int(duration.Hours()))
	}
	return fmt.Sprintf("%dd", int(duration.Hours()/24))
}

func (a *application) statusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status [name]",
		Short: "Show sandbox status and recent conditions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			name, err := a.resolveSandboxName(command.Context(), client, args)
			if err != nil {
				return err
			}
			value, err := client.Sandbox(command.Context(), name)
			if err != nil {
				return err
			}
			if a.json {
				return writeJSON(a.options.Out, value)
			}
			writeSandboxStatus(a.options.Out, value)
			return nil
		},
	}
}

func writeSandboxStatus(out io.Writer, value Sandbox) {
	source := "empty"
	if value.Source.Type == "git" {
		source = value.Source.RepositoryID
		if value.Source.RefName != "" {
			source += "@" + value.Source.RefName
		}
	}
	_, _ = fmt.Fprintf(out,
		"Name: %s\nState: %s\nDesired state: %s\nTemplate: %s@%s\nProfile: %s\nSource: %s\nCreated: %s\n",
		value.Name, value.Phase, value.DesiredState, value.Template.Name, value.Template.Version,
		value.Profile.Name, source, value.CreatedAt.Format(time.RFC3339))
	if value.LastActivityTime != nil {
		_, _ = fmt.Fprintf(out, "Last activity: %s\n", value.LastActivityTime.Format(time.RFC3339))
	}
	if value.IdleDeadline != nil {
		_, _ = fmt.Fprintf(out, "Idle deadline: %s\n", value.IdleDeadline.Format(time.RFC3339))
	}
	if value.RetentionDeadline != nil {
		_, _ = fmt.Fprintf(out, "Retention deadline: %s\n", value.RetentionDeadline.Format(time.RFC3339))
	}
	if len(value.Conditions) > 0 {
		_, _ = fmt.Fprintln(out, "Conditions:")
		for _, condition := range value.Conditions {
			_, _ = fmt.Fprintf(out, "  %s=%s %s: %s\n",
				condition.Type, condition.Status, condition.Reason, condition.Message)
		}
	}
}

func (a *application) stopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [name]",
		Short: "Stop a sandbox while retaining its workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			name, err := a.resolveSandboxName(command.Context(), client, args)
			if err != nil {
				return err
			}
			value, err := client.StopSandbox(command.Context(), name)
			if err != nil {
				return err
			}
			return a.writeMutation(value, fmt.Sprintf("Stop requested for %s", name))
		},
	}
}

func (a *application) resumeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "resume [name]",
		Short: "Resume a stopped sandbox",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			name, err := a.resolveSandboxName(command.Context(), client, args)
			if err != nil {
				return err
			}
			value, err := client.ResumeSandbox(command.Context(), name)
			if err != nil {
				return err
			}
			ready, err := a.waitReady(command.Context(), client, value)
			if err != nil {
				return err
			}
			return a.writeMutation(ready, fmt.Sprintf("Sandbox %s is Running", name))
		},
	}
}

func (a *application) deleteCommand() *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:   "delete [name]",
		Short: "Permanently delete a sandbox",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			name, err := a.resolveSandboxName(command.Context(), client, args)
			if err != nil {
				return err
			}
			if !yes {
				confirmed, confirmErr := a.prompt.Confirm(fmt.Sprintf("Permanently delete sandbox %s?", name))
				if confirmErr != nil {
					return confirmErr
				}
				if !confirmed {
					if !a.json {
						_, _ = fmt.Fprintln(a.options.Out, "Delete canceled")
					}
					return nil
				}
			}
			err = client.DeleteSandbox(command.Context(), name)
			var cliErr *Error
			if err != nil && (!errors.As(err, &cliErr) || cliErr.ExitCode != ExitNotFound) {
				return err
			}
			if a.json {
				return writeJSON(a.options.Out, map[string]any{"deleted": true, "name": name})
			}
			_, _ = fmt.Fprintf(a.options.Out, "Deleted %s\n", name)
			return nil
		},
	}
	command.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	return command
}

func (a *application) writeMutation(value Sandbox, message string) error {
	if a.json {
		return writeJSON(a.options.Out, value)
	}
	_, _ = fmt.Fprintln(a.options.Out, message)
	return nil
}

func (a *application) resolveSandboxName(ctx context.Context, client *Client, args []string) (string, error) {
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		return args[0], nil
	}
	repository, err := a.options.Resolver.Current(ctx, a.prompt)
	if err != nil {
		items, listErr := client.Sandboxes(ctx, true)
		choices := make([]string, 0, len(items))
		if listErr == nil {
			for _, item := range items {
				choices = append(choices, item.Name)
			}
			sort.Strings(choices)
		}
		return "", cliError(ExitInvalid, "sandbox_name_required",
			"NAME is required outside a current GitHub repository",
			map[string]any{"choices": choices})
	}
	items, err := client.Sandboxes(ctx, true)
	if err != nil {
		return "", err
	}
	matches := make([]Sandbox, 0)
	for _, item := range items {
		if sandboxMatchesRepository(item, repository) {
			matches = append(matches, item)
		}
	}
	if len(matches) == 0 {
		return "", cliError(ExitNotFound, "sandbox_not_found",
			fmt.Sprintf("no sandbox is associated with %s", repository.ID), nil)
	}
	selected, err := a.selectSandbox("Multiple sandboxes are associated with the current repository; choose one", matches)
	if err != nil {
		return "", err
	}
	return selected.Name, nil
}

func (a *application) selectSandbox(message string, items []Sandbox) (Sandbox, error) {
	if len(items) == 1 {
		return items[0], nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	choices := make([]string, len(items))
	byName := make(map[string]Sandbox, len(items))
	for index, item := range items {
		choices[index] = item.Name
		byName[item.Name] = item
	}
	choice, err := a.prompt.Select(message, choices)
	if err != nil {
		return Sandbox{}, err
	}
	return byName[choice], nil
}
