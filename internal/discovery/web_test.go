package discovery

import (
	"net/http"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestIdentifyProduct(t *testing.T) {
	tests := []struct{ body, want string }{{`<title>GitLab</title>`, "GitLab"}, {`Sonatype Nexus Repository Manager`, "Nexus Repository"}, {`<meta name="generator" content="Wiki.js">`, "Wiki.js"}, {`plain nginx`, ""}}
	for _, test := range tests {
		got, _ := identifyProduct(http.Header{}, test.body)
		if got != test.want {
			t.Errorf("%q: got %q want %q", test.body, got, test.want)
		}
	}
}

func TestIdentifyGitLabFromRedirectHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("X-Gitlab-Meta", `{"correlation_id":"test","version":"1"}`)
	header.Set("Location", "https://gitlab.example.org/users/sign_in")
	product, evidence := identifyProduct(header, "redirected")
	if product != "GitLab" || evidence == "" {
		t.Fatalf("product=%q evidence=%q", product, evidence)
	}
}

func TestMatchServiceUsesVMAndProductNames(t *testing.T) {
	services := []*model.DiscoveredService{{ID: "git", Product: "GitLab", VMName: "adv-gitlab"}}
	for _, folder := range []string{"gitlab-nightly", "adv_gitlab_backup"} {
		if got := matchService(folder, services); got != "git" {
			t.Errorf("%s matched %q", folder, got)
		}
	}
	if got := matchService("unrelated", services); got != "" {
		t.Errorf("unexpected match %q", got)
	}
}
