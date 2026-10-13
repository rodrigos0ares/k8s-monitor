package web

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rodrigos0ares/k8s-monitor/internal/storage"
)

var (
	replicaSetPodPattern = regexp.MustCompile(`^(.*)-[a-f0-9]{9,10}-[a-z0-9]{5}$`)
	statefulSetPodPattern = regexp.MustCompile(`^(.*)-\d+$`)
)

type repoIndex struct {
	root    string
	orgs    map[string]*repoOrg
	byName  map[string]RepoReference
	byOwner map[string]string
}

type repoOrg struct {
	DirName string
	Owner   string
	Repos   map[string]RepoReference
	Infra   *RepoReference
}

type RepoReference struct {
	Name          string
	Owner         string
	Path          string
	Remote        string
	VSCodeURL     string
	VSCodeCommand string
	GitHubURL     string
	OrgDir        string
}

type DeploymentIssueGroup struct {
	Context         string
	Namespace       string
	Deployment      string
	HighestSeverity string
	TotalCount      int
	Repo            *RepoReference
	InfraRepo       *RepoReference
	Tabs            []DeploymentIssueTab
}

type DeploymentIssueTab struct {
	ID         string
	Title      string
	Severity   string
	Count      int
	Pod        string
	SampleLog  string
	Plan       FixPlan
	Signature  string
	Resolution string
}

type FixPlan struct {
	Summary            string
	LikelyCause        string
	Steps              []string
	Commands           []string
	SearchTerms        []string
	AppRepo            *RepoReference
	InfraRepo          *RepoReference
	RepoSearchCommands []string
}

type issuePattern struct {
	Name         string
	Summary      string
	LikelyCause  string
	Steps        []string
	SearchTerms  []string
	Resolution   string
	SignatureKey string
}

func newRepoIndex(root string) *repoIndex {
	if root == "" {
		return nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	idx := &repoIndex{
		root:    root,
		orgs:    make(map[string]*repoOrg),
		byName:  make(map[string]RepoReference),
		byOwner: make(map[string]string),
	}

	for _, orgEntry := range entries {
		if !orgEntry.IsDir() {
			continue
		}

		orgPath := filepath.Join(root, orgEntry.Name())
		repoEntries, err := os.ReadDir(orgPath)
		if err != nil {
			continue
		}

		org := &repoOrg{
			DirName: orgEntry.Name(),
			Repos:   make(map[string]RepoReference),
		}

		for _, repoEntry := range repoEntries {
			if !repoEntry.IsDir() {
				continue
			}

			repoPath := filepath.Join(orgPath, repoEntry.Name())
			if !looksLikeRepo(repoPath) {
				continue
			}

			ref := RepoReference{
				Name:          repoEntry.Name(),
				Path:          repoPath,
				VSCodeURL:     buildVSCodeURL(repoPath),
				VSCodeCommand: fmt.Sprintf("code %q", repoPath),
				OrgDir:        orgEntry.Name(),
			}

			if remote, owner, repoName := readGitRemote(repoPath); remote != "" {
				ref.Remote = remote
				ref.Owner = owner
				if repoName != "" {
					ref.GitHubURL = fmt.Sprintf("https://github.com/%s/%s", owner, repoName)
					idx.byOwner[normalizeName(orgEntry.Name())] = owner
				}
			}

			key := normalizeName(ref.Name)
			org.Repos[key] = ref
			idx.byName[key] = ref

			if key == "infra" {
				infra := ref
				org.Infra = &infra
			}
		}

		if len(org.Repos) > 0 || org.Infra != nil {
			if org.Owner == "" {
				org.Owner = idx.byOwner[normalizeName(org.DirName)]
			}
			idx.orgs[normalizeName(org.DirName)] = org
		}
	}

	return idx
}

func discoverProjectsRoot() string {
	if root := os.Getenv("K8S_MONITOR_PROJECTS_ROOT"); root != "" {
		return root
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "dev", "projects")
}

func looksLikeRepo(path string) bool {
	markers := []string{".git", "package.json", "go.mod", "pyproject.toml", "Cargo.toml"}
	for _, marker := range markers {
		if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
			return true
		}
	}
	return false
}

func readGitRemote(repoPath string) (remote, owner, repo string) {
	configPath := filepath.Join(repoPath, ".git", "config")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", "", ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "url =") {
			continue
		}
		remote = strings.TrimSpace(strings.TrimPrefix(line, "url ="))
		owner, repo = parseGitHubRemote(remote)
		return remote, owner, repo
	}

	return "", "", ""
}

func parseGitHubRemote(remote string) (owner, repo string) {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimSuffix(remote, ".git")
	remote = strings.TrimSuffix(remote, "/")

	switch {
	case strings.HasPrefix(remote, "git@github.com:"):
		remote = strings.TrimPrefix(remote, "git@github.com:")
	case strings.HasPrefix(remote, "https://github.com/"):
		remote = strings.TrimPrefix(remote, "https://github.com/")
	default:
		return "", ""
	}

	parts := strings.Split(remote, "/")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func buildVSCodeURL(path string) string {
	return (&url.URL{Scheme: "vscode", Host: "file", Path: path}).String()
}

func buildDeploymentIssues(errors []storage.ErrorSummary, idx *repoIndex) []DeploymentIssueGroup {
	groups := make(map[string]*DeploymentIssueGroup)

	for _, entry := range errors {
		deployment := deploymentNameFromPod(entry.Pod)
		key := strings.Join([]string{entry.Context, entry.Namespace, deployment}, "|")

		group := groups[key]
		if group == nil {
			repo, infra := findRepoMatches(idx, deployment, entry.Context, entry.Namespace)
			group = &DeploymentIssueGroup{
				Context:         entry.Context,
				Namespace:       entry.Namespace,
				Deployment:      deployment,
				HighestSeverity: entry.Severity,
				Repo:            repo,
				InfraRepo:       infra,
			}
			groups[key] = group
		}

		if severityRank(entry.Severity) > severityRank(group.HighestSeverity) {
			group.HighestSeverity = entry.Severity
		}
		group.TotalCount += entry.Count

		pattern := classifyIssue(entry, deployment)
		group.Tabs = append(group.Tabs, DeploymentIssueTab{
			ID:         fmt.Sprintf("tab-%s-%d", slugify(key), len(group.Tabs)),
			Title:      pattern.Name,
			Severity:   entry.Severity,
			Count:      entry.Count,
			Pod:        entry.Pod,
			SampleLog:  entry.SampleLog,
			Signature:  pattern.SignatureKey,
			Resolution: pattern.Resolution,
			Plan:       buildFixPlan(entry, deployment, pattern, group.Repo, group.InfraRepo, idx),
		})
	}

	issues := make([]DeploymentIssueGroup, 0, len(groups))
	for _, group := range groups {
		sort.Slice(group.Tabs, func(i, j int) bool {
			if group.Tabs[i].Count == group.Tabs[j].Count {
				return severityRank(group.Tabs[i].Severity) > severityRank(group.Tabs[j].Severity)
			}
			return group.Tabs[i].Count > group.Tabs[j].Count
		})
		issues = append(issues, *group)
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].TotalCount == issues[j].TotalCount {
			return severityRank(issues[i].HighestSeverity) > severityRank(issues[j].HighestSeverity)
		}
		return issues[i].TotalCount > issues[j].TotalCount
	})

	return issues
}

func findRepoMatches(idx *repoIndex, deployment, context, namespace string) (*RepoReference, *RepoReference) {
	if idx == nil {
		return nil, nil
	}

	org := idx.findOrg(context, namespace)
	key := normalizeName(deployment)
	if org != nil {
		if repo, ok := org.Repos[key]; ok {
			repoCopy := repo
			if org.Infra != nil {
				infraCopy := *org.Infra
				return &repoCopy, &infraCopy
			}
			return &repoCopy, nil
		}
		if org.Infra != nil {
			infraCopy := *org.Infra
			return nil, &infraCopy
		}
	}

	if repo, ok := idx.byName[key]; ok {
		repoCopy := repo
		return &repoCopy, nil
	}

	return nil, nil
}

func (idx *repoIndex) findOrg(context, namespace string) *repoOrg {
	for _, candidate := range []string{namespace, context} {
		if org, ok := idx.orgs[normalizeName(candidate)]; ok {
			return org
		}
	}
	return nil
}

func deploymentNameFromPod(pod string) string {
	if match := replicaSetPodPattern.FindStringSubmatch(pod); len(match) == 2 {
		return match[1]
	}
	if match := statefulSetPodPattern.FindStringSubmatch(pod); len(match) == 2 {
		return match[1]
	}
	return pod
}

func classifyIssue(entry storage.ErrorSummary, deployment string) issuePattern {
	lower := strings.ToLower(entry.SampleLog)

	switch {
	case strings.Contains(lower, "imagepullbackoff") ||
		strings.Contains(lower, "errimagepull") ||
		strings.Contains(lower, "failed to pull image") ||
		strings.Contains(lower, "oauth token") ||
		strings.Contains(lower, "pull access denied") ||
		strings.Contains(lower, "no basic auth credentials"):
		return issuePattern{
			Name:        "Image pull and registry access",
			Summary:     "The deployment is failing before startup because Kubernetes cannot fetch its image.",
			LikelyCause: "Registry credentials, image visibility, or the image tag for this deployment is out of sync with the cluster manifest.",
			Steps: []string{
				"Confirm the deployment image tag and imagePullSecrets on the Kubernetes manifest.",
				"Validate the registry credentials and package visibility before changing application code.",
				"Once the pull succeeds, recycle the deployment and verify the new pod reaches Running.",
			},
			SearchTerms:  []string{"imagePullSecrets", deployment, "registrysecret", "ghcr.io"},
			Resolution:   "Check manifests and registry auth before touching runtime code.",
			SignatureKey: "image-pull",
		}
	case strings.Contains(lower, "crashloopbackoff") ||
		strings.Contains(lower, "panic") ||
		strings.Contains(lower, "exception") ||
		strings.Contains(lower, "traceback") ||
		strings.Contains(lower, "segmentation fault"):
		return issuePattern{
			Name:        "Application crash on startup",
			Summary:     "The pod is starting, but the application process is exiting repeatedly.",
			LikelyCause: "A runtime exception, failed initialization path, or missing configuration is crashing the process after image pull.",
			Steps: []string{
				"Capture the first crashing stack trace or panic from the newest pod logs.",
				"Open the app repo in VS Code and search for the crashing symbol or exception message.",
				"Patch the initialization path or missing config, then rebuild and redeploy the deployment.",
			},
			SearchTerms:  []string{deployment, "panic", "exception", "startup"},
			Resolution:   "Start from the first crashing stack trace, not the tail of the restart noise.",
			SignatureKey: "crash-loop",
		}
	case strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "econnrefused") ||
		strings.Contains(lower, "i/o timeout"):
		return issuePattern{
			Name:        "Upstream connectivity failure",
			Summary:     "The deployment is running into network or service dependency failures.",
			LikelyCause: "An upstream service endpoint, service DNS, port, or env-based connection string is incorrect or unavailable.",
			Steps: []string{
				"Identify which upstream service or URL the deployment is trying to reach.",
				"Open the app and infra repos in VS Code to compare env values, service names, and ports.",
				"Verify the dependency from inside the cluster before changing retry logic.",
			},
			SearchTerms:  []string{deployment, "timeout", "connection refused", "service"},
			Resolution:   "Treat this as a config or dependency path problem before blaming the application loop.",
			SignatureKey: "connectivity",
		}
	case strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "unauthorized"):
		return issuePattern{
			Name:        "Permissions or auth failure",
			Summary:     "The deployment is blocked by an authorization or permission boundary.",
			LikelyCause: "A token, secret, service account, or external API credential is missing the scope needed by this workload.",
			Steps: []string{
				"Pin down whether the denial comes from Kubernetes RBAC, a secret, or an external provider.",
				"Search the deployment manifest and app config in VS Code for the failing credential path.",
				"Rotate or widen the credential only after confirming the exact denied action.",
			},
			SearchTerms:  []string{deployment, "forbidden", "unauthorized", "serviceAccount"},
			Resolution:   "Capture the denied action first, then update the smallest credential or RBAC scope that fixes it.",
			SignatureKey: "auth",
		}
	case strings.Contains(lower, "oomkilled") || strings.Contains(lower, "out of memory"):
		return issuePattern{
			Name:        "Memory pressure or OOM",
			Summary:     "The workload is exhausting the memory envelope it was deployed with.",
			LikelyCause: "Requests and limits are too low for the current traffic or the code path is retaining too much memory.",
			Steps: []string{
				"Compare the workload requests and limits with recent usage and restart timing.",
				"Open the app repo in VS Code and inspect heavy startup, caching, and batching paths.",
				"Raise limits only if the code path is expected; otherwise fix the memory growth first.",
			},
			SearchTerms:  []string{deployment, "memory", "oom", "cache"},
			Resolution:   "Use the smallest limit increase that restores service while you isolate the hot path.",
			SignatureKey: "oom",
		}
	default:
		return issuePattern{
			Name:        "Repeated runtime errors",
			Summary:     "This deployment has recurring error or warning logs that need a code and config review.",
			LikelyCause: "The log sample points to an application-specific failure path that needs inspection in the owning repo and manifest.",
			Steps: []string{
				"Use the sample log to find the exact call site or config path in the repo.",
				"Compare the deployment manifest, image tag, and environment with the code expectation.",
				"Patch the narrow failing path first, then redeploy and watch the next pod cycle.",
			},
			SearchTerms:  []string{deployment, entry.Severity, "error", firstWords(entry.SampleLog, 6)},
			Resolution:   "Search from the literal error text to the nearest owning code path before changing surrounding systems.",
			SignatureKey: "generic",
		}
	}
}

func buildFixPlan(entry storage.ErrorSummary, deployment string, pattern issuePattern, repo, infra *RepoReference, idx *repoIndex) FixPlan {
	commands := []string{
		fmt.Sprintf("kubectl describe deployment/%s -n %s", deployment, entry.Namespace),
		fmt.Sprintf("kubectl describe pod/%s -n %s", entry.Pod, entry.Namespace),
		fmt.Sprintf("kubectl logs pod/%s -n %s --tail=200", entry.Pod, entry.Namespace),
	}

	switch pattern.SignatureKey {
	case "image-pull":
		commands = append(commands,
			fmt.Sprintf("kubectl get deployment/%s -n %s -o yaml", deployment, entry.Namespace),
			fmt.Sprintf("kubectl get secret registrysecret -n %s -o yaml", entry.Namespace),
		)
	case "connectivity":
		commands = append(commands,
			fmt.Sprintf("kubectl get svc -n %s", entry.Namespace),
			fmt.Sprintf("kubectl get endpoints -n %s", entry.Namespace),
		)
	case "auth":
		commands = append(commands,
			fmt.Sprintf("kubectl get serviceaccount default -n %s -o yaml", entry.Namespace),
			fmt.Sprintf("kubectl auth can-i --namespace %s get pods", entry.Namespace),
		)
	}

	if repo != nil {
		commands = append(commands, repo.VSCodeCommand)
	}
	if infra != nil {
		commands = append(commands, infra.VSCodeCommand)
	}

	plan := FixPlan{
		Summary:     pattern.Summary,
		LikelyCause: pattern.LikelyCause,
		Steps:       append([]string{}, pattern.Steps...),
		Commands:    commands,
		SearchTerms: compactSearchTerms(pattern.SearchTerms),
		AppRepo:     repo,
		InfraRepo:   infra,
	}

	if repo == nil {
		plan.RepoSearchCommands = suggestRepoSearchCommands(idx, deployment, entry.Context, entry.Namespace)
		if len(plan.RepoSearchCommands) > 0 {
			plan.Steps = append(plan.Steps,
				"If the owning app repo is not local yet, search or clone it with gh before patching.",
			)
		}
	}

	return plan
}

func suggestRepoSearchCommands(idx *repoIndex, deployment, context, namespace string) []string {
	if idx == nil {
		return nil
	}

	var owner, orgDir string
	if org := idx.findOrg(context, namespace); org != nil {
		owner = org.Owner
		orgDir = org.DirName
	}
	if owner == "" && orgDir != "" {
		owner = idx.byOwner[normalizeName(orgDir)]
	}
	if owner == "" {
		return nil
	}

	targetDir := filepath.Join(idx.root, orgDir, deployment)
	return []string{
		fmt.Sprintf("gh search repos %q --owner %s --limit 5", deployment, owner),
		fmt.Sprintf("gh repo clone %s/%s %q", owner, deployment, targetDir),
	}
}

func firstWords(text string, limit int) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	if len(fields) > limit {
		fields = fields[:limit]
	}
	return strings.Join(fields, " ")
}

func compactSearchTerms(terms []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		result = append(result, term)
	}
	return result
}

func severityRank(severity string) int {
	switch strings.ToLower(severity) {
	case "critical":
		return 5
	case "error", "high":
		return 4
	case "warning", "medium":
		return 3
	case "low":
		return 2
	default:
		return 1
	}
}

func normalizeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("_", "-", " ", "-", ".", "-").Replace(value)
	value = regexp.MustCompile(`-+`).ReplaceAllString(value, "-")
	return strings.Trim(value, "-")
}

func slugify(value string) string {
	value = normalizeName(value)
	value = strings.ReplaceAll(value, "|", "-")
	return value
}