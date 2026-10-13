package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
)

func TestDeploymentNameFromPod(t *testing.T) {
	tests := map[string]string{
		"dai-web-video-79b77bb849-ktd8b": "dai-web-video",
		"postgresql-0":                    "postgresql",
		"plain-pod-name":                  "plain-pod-name",
	}

	for pod, want := range tests {
		if got := deploymentNameFromPod(pod); got != want {
			t.Fatalf("deploymentNameFromPod(%q) = %q, want %q", pod, got, want)
		}
	}
}

func TestBuildDeploymentIssuesUsesLocalRepoAndInfra(t *testing.T) {
	root := t.TempDir()
	createRepo(t, root, "divinia", "dai-web-video", "https://github.com/divinia-ai/dai-web-video.git")
	createRepo(t, root, "divinia", "infra", "https://github.com/divinia-ai/dai-terraform-scripts.git")

	issues := buildDeploymentIssues([]storage.ErrorSummary{{
		Context:   "divinia",
		Namespace: "divinia",
		Pod:       "dai-web-video-79b77bb849-ktd8b",
		Severity:  "error",
		Count:     12,
		SampleLog: "Failed to pull image \"ghcr.io/divinia-ai/dai-web-video:4ba1714\": failed to fetch oauth token",
	}}, newRepoIndex(root))

	if len(issues) != 1 {
		t.Fatalf("expected 1 grouped issue, got %d", len(issues))
	}

	issue := issues[0]
	if issue.Deployment != "dai-web-video" {
		t.Fatalf("expected deployment dai-web-video, got %q", issue.Deployment)
	}
	if issue.Repo == nil || issue.Repo.Name != "dai-web-video" {
		t.Fatalf("expected app repo match, got %#v", issue.Repo)
	}
	if issue.InfraRepo == nil || issue.InfraRepo.Name != "infra" {
		t.Fatalf("expected infra repo match, got %#v", issue.InfraRepo)
	}
	if len(issue.Tabs) != 1 {
		t.Fatalf("expected 1 issue tab, got %d", len(issue.Tabs))
	}
	if got := issue.Tabs[0].Plan.AppRepo; got == nil || !strings.Contains(got.VSCodeURL, "vscode://file/") {
		t.Fatalf("expected VS Code repo link, got %#v", got)
	}
	if len(issue.Tabs[0].Plan.RepoSearchCommands) != 0 {
		t.Fatalf("expected no gh search commands when local repo exists, got %#v", issue.Tabs[0].Plan.RepoSearchCommands)
	}
}

func TestBuildDeploymentIssuesSuggestsGhCommandsWhenRepoMissing(t *testing.T) {
	root := t.TempDir()
	createRepo(t, root, "divinia", "infra", "https://github.com/divinia-ai/dai-terraform-scripts.git")

	issues := buildDeploymentIssues([]storage.ErrorSummary{{
		Context:   "divinia",
		Namespace: "divinia",
		Pod:       "dai-web-video-api-7fbcbf95db-9tnsg",
		Severity:  "error",
		Count:     3,
		SampleLog: "ImagePullBackOff: failed to pull image",
	}}, newRepoIndex(root))

	if len(issues) != 1 {
		t.Fatalf("expected 1 grouped issue, got %d", len(issues))
	}

	plan := issues[0].Tabs[0].Plan
	if len(plan.RepoSearchCommands) != 2 {
		t.Fatalf("expected gh search and clone commands, got %#v", plan.RepoSearchCommands)
	}
	if !strings.Contains(plan.RepoSearchCommands[0], "gh search repos") {
		t.Fatalf("expected gh repo search command, got %q", plan.RepoSearchCommands[0])
	}
	if !strings.Contains(plan.RepoSearchCommands[1], "gh repo clone divinia-ai/dai-web-video-api") {
		t.Fatalf("expected gh repo clone hint, got %q", plan.RepoSearchCommands[1])
	}
}

func createRepo(t *testing.T, root, org, name, remote string) {
	t.Helper()
	path := filepath.Join(root, org, name, ".git")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	config := "[remote \"origin\"]\n\turl = " + remote + "\n"
	if err := os.WriteFile(filepath.Join(path, "config"), []byte(config), 0o644); err != nil {
		t.Fatalf("write git config: %v", err)
	}
}