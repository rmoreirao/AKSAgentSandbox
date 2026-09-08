package cli

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type Prompter struct {
	terminal bool
	scanner  *bufio.Scanner
	out      io.Writer
}

func NewPrompter(in io.Reader, out io.Writer, terminal bool) *Prompter {
	return &Prompter{terminal: terminal, scanner: bufio.NewScanner(in), out: out}
}

func (p *Prompter) Select(message string, choices []string) (string, error) {
	if len(choices) == 0 {
		return "", invalid(message + ": no choices are available")
	}
	if !p.terminal {
		return "", cliError(ExitInvalid, "choice_required", message,
			map[string]any{"choices": choices})
	}
	_, _ = fmt.Fprintln(p.out, message)
	for index, choice := range choices {
		_, _ = fmt.Fprintf(p.out, "  %d) %s\n", index+1, choice)
	}
	_, _ = fmt.Fprint(p.out, "Select: ")
	if !p.scanner.Scan() {
		return "", invalid("no selection was provided")
	}
	value := strings.TrimSpace(p.scanner.Text())
	for _, choice := range choices {
		if strings.EqualFold(value, choice) {
			return choice, nil
		}
	}
	index, err := strconv.Atoi(value)
	if err == nil && index >= 1 && index <= len(choices) {
		return choices[index-1], nil
	}
	return "", cliError(ExitInvalid, "invalid_choice", "selection is not one of the available choices",
		map[string]any{"choices": choices})
}

func (p *Prompter) Confirm(message string) (bool, error) {
	if !p.terminal {
		return false, cliError(ExitInvalid, "confirmation_required",
			message+"; use --yes in noninteractive environments", map[string]any{"choices": []string{"yes", "no"}})
	}
	_, _ = fmt.Fprintf(p.out, "%s [y/N]: ", message)
	if !p.scanner.Scan() {
		return false, invalid("no confirmation was provided")
	}
	switch strings.ToLower(strings.TrimSpace(p.scanner.Text())) {
	case "y", "yes":
		return true, nil
	case "", "n", "no":
		return false, nil
	default:
		return false, cliError(ExitInvalid, "invalid_choice", "confirmation must be yes or no",
			map[string]any{"choices": []string{"yes", "no"}})
	}
}
