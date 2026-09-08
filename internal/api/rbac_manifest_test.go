package api

import (
	"os"
	"strings"
	"testing"
)

func TestManagementRBACIsNarrow(t *testing.T) {
	apiRBAC, err := os.ReadFile(`..\..\deploy\kustomize\base\api-rbac.yaml`)
	if err != nil {
		t.Fatal(err)
	}
	apiText := string(apiRBAC)
	for _, forbidden := range []string{"resources: [secrets]", "resources: [nodes]", "resources: [pods]", "verbs: [*]"} {
		if strings.Contains(apiText, forbidden) {
			t.Fatalf("API RBAC includes forbidden permission %q", forbidden)
		}
	}
	for _, required := range []string{"resources: [devsandboxes]", "resources: [devsandboxtemplates]", "resources: [leases]"} {
		if !strings.Contains(apiText, required) {
			t.Fatalf("API RBAC is missing %q", required)
		}
	}

	brokerRBAC, err := os.ReadFile(`..\..\deploy\kustomize\base\broker-rbac.yaml`)
	if err != nil {
		t.Fatal(err)
	}
	brokerText := string(brokerRBAC)
	if strings.Contains(brokerText, "resources: [secrets]") ||
		strings.Contains(brokerText, "verbs: [get, list") ||
		strings.Contains(brokerText, "delete") {
		t.Fatal("broker can read unrelated resources or mutate domain resources")
	}

	accounts, err := os.ReadFile(`..\..\deploy\kustomize\base\serviceaccounts.yaml`)
	if err != nil {
		t.Fatal(err)
	}
	accountText := string(accounts)
	web := accountText[strings.Index(accountText, "name: devsandbox-web"):]
	web = web[:strings.Index(web, "---")]
	if !strings.Contains(web, "automountServiceAccountToken: false") {
		t.Fatal("web gateway service account mounts a Kubernetes token")
	}
	if strings.Contains(apiText+brokerText, "name: devsandbox-web") {
		t.Fatal("web gateway has an RBAC binding")
	}
}
