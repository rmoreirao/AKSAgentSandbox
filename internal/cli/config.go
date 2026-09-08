package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var environmentKeyPattern = regexp.MustCompile(`^DEVSANDBOX_[A-Z0-9_]+$`)

func loadLocalEnvironment() (func(), error) {
	if explicit := strings.TrimSpace(os.Getenv("DEVSANDBOX_ENV_FILE")); explicit != "" {
		return applyEnvironmentFile(explicit, true)
	}

	if executable, err := os.Executable(); err == nil {
		return applyEnvironmentFile(filepath.Join(filepath.Dir(executable), ".env"), false)
	}
	return func() {}, nil
}

func applyEnvironmentFile(path string, required bool) (func(), error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return func() {}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	values, err := parseEnvironment(file)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	type previousValue struct {
		value  string
		exists bool
	}
	previous := map[string]previousValue{}
	restore := func() {
		for key, prior := range previous {
			if prior.exists {
				_ = os.Setenv(key, prior.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
	for key, value := range values {
		prior, exists := os.LookupEnv(key)
		if exists {
			continue
		}
		previous[key] = previousValue{value: prior, exists: exists}
		if err := os.Setenv(key, value); err != nil {
			restore()
			return nil, fmt.Errorf("set %s from %s: %w", key, path, err)
		}
	}
	return restore, nil
}

func parseEnvironment(reader io.Reader) (map[string]string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4<<10), 256<<10)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, rawValue, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !environmentKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("line %d must define a DEVSANDBOX_* variable", lineNumber)
		}
		value, err := parseEnvironmentValue(strings.TrimSpace(rawValue))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func parseEnvironmentValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	switch value[0] {
	case '"':
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", errors.New("invalid double-quoted value")
		}
		return decoded, nil
	case '\'':
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", errors.New("invalid single-quoted value")
		}
		return value[1 : len(value)-1], nil
	default:
		if strings.ContainsAny(value, "\r\n") {
			return "", errors.New("unquoted value contains a newline")
		}
		return value, nil
	}
}
