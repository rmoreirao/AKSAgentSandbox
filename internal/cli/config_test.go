package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvironment(t *testing.T) {
	values, err := parseEnvironment(strings.NewReader(`
# generated configuration
DEVSANDBOX_API_URL=https://api.example
export DEVSANDBOX_AUTH_MODE="github-cli-static"
DEVSANDBOX_GITHUB_LOGIN=octocat
DEVSANDBOX_DEFAULT_TEMPLATE='standard'
`))
	if err != nil {
		t.Fatal(err)
	}
	if values["DEVSANDBOX_API_URL"] != "https://api.example" ||
		values["DEVSANDBOX_AUTH_MODE"] != "github-cli-static" ||
		values["DEVSANDBOX_GITHUB_LOGIN"] != "octocat" ||
		values["DEVSANDBOX_DEFAULT_TEMPLATE"] != "standard" {
		t.Fatalf("values = %#v", values)
	}
}

func TestParseEnvironmentRejectsUnscopedAndMalformedValues(t *testing.T) {
	for _, input := range []string{
		"PATH=unexpected\n",
		"DEVSANDBOX_API_URL\n",
		"DEVSANDBOX_API_URL=\"unterminated\n",
	} {
		if _, err := parseEnvironment(strings.NewReader(input)); err == nil {
			t.Fatalf("parseEnvironment(%q) succeeded", input)
		}
	}
}

func TestApplyEnvironmentFilePreservesExplicitValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(
		"DEVSANDBOX_API_URL=https://from-file.example\n"+
			"DEVSANDBOX_TEST_ENV_VALUE=loaded\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVSANDBOX_API_URL", "https://explicit.example")
	_ = os.Unsetenv("DEVSANDBOX_TEST_ENV_VALUE")
	t.Cleanup(func() { _ = os.Unsetenv("DEVSANDBOX_TEST_ENV_VALUE") })

	restore, err := applyEnvironmentFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if value := os.Getenv("DEVSANDBOX_API_URL"); value != "https://explicit.example" {
		t.Fatalf("explicit value overwritten: %q", value)
	}
	if value := os.Getenv("DEVSANDBOX_TEST_ENV_VALUE"); value != "loaded" {
		t.Fatalf("file value = %q", value)
	}
}

func TestApplyEnvironmentFileRestoresLoadedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("DEVSANDBOX_TEST_RESTORE=loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Unsetenv("DEVSANDBOX_TEST_RESTORE")
	t.Cleanup(func() { _ = os.Unsetenv("DEVSANDBOX_TEST_RESTORE") })

	restore, err := applyEnvironmentFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if value := os.Getenv("DEVSANDBOX_TEST_RESTORE"); value != "loaded" {
		t.Fatalf("loaded value = %q", value)
	}
	restore()
	if _, exists := os.LookupEnv("DEVSANDBOX_TEST_RESTORE"); exists {
		t.Fatal("loaded value was not removed")
	}
}

func TestExecuteRestoresExplicitEnvironmentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("DEVSANDBOX_TEST_EXECUTE=loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVSANDBOX_ENV_FILE", path)
	_ = os.Unsetenv("DEVSANDBOX_TEST_EXECUTE")
	t.Cleanup(func() { _ = os.Unsetenv("DEVSANDBOX_TEST_EXECUTE") })

	output := &bytes.Buffer{}
	if code := Execute([]string{"--version"}, Options{Out: output, Err: output, Version: "test"}); code != 0 {
		t.Fatalf("exit=%d output=%s", code, output.String())
	}
	if _, exists := os.LookupEnv("DEVSANDBOX_TEST_EXECUTE"); exists {
		t.Fatal("Execute did not restore the process environment")
	}
}
