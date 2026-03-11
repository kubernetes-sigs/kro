package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	githubapi "github.com/google/go-github/v74/github"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultAPIBaseURL = "https://api.github.com"
	defaultOwner      = "kubernetes-sigs"
	defaultRepo       = "kro"
	defaultKREPLabel  = "kind/krep"
	defaultOutputPath = "website/src/data/generated/krep-roadmap.json"
)

type crawlConfig struct {
	APIBaseURL string
	Owner      string
	Repo       string
	Label      string
	OutputPath string
	Timeout    time.Duration
}

type outputDocument struct {
	SchemaVersion int           `json:"schemaVersion"`
	GeneratedAt   string        `json:"generatedAt"`
	Repository    string        `json:"repository"`
	Label         string        `json:"label"`
	Releases      []releaseLane `json:"releases"`
	Items         []roadmapItem `json:"items"`
}

type releaseLane struct {
	Release   string        `json:"release"`
	URL       string        `json:"url"`
	Phase     string        `json:"phase"`
	Milestone milestoneMeta `json:"milestone"`
}

type roadmapItem struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Summary    string         `json:"summary"`
	Status     string         `json:"status"`
	Owners     []string       `json:"owners"`
	Release    string         `json:"release,omitempty"`
	ProposalPR int            `json:"proposalPr"`
	Milestone  *milestoneMeta `json:"milestone,omitempty"`
	GitHub     githubMeta     `json:"github"`
}

type milestoneMeta struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	URL    string `json:"url"`
}

type githubMeta struct {
	State     string   `json:"state"`
	Draft     bool     `json:"draft"`
	Merged    bool     `json:"merged"`
	MergedAt  string   `json:"mergedAt,omitempty"`
	Author    string   `json:"author"`
	Assignees []string `json:"assignees"`
	Labels    []string `json:"labels"`
}

type githubClient struct {
	client *githubapi.Client
}

var krepTitlePattern = regexp.MustCompile(`(?i)\bKREP[- ]?0*(\d+)\b`)

func main() {
	cfg := crawlConfig{
		APIBaseURL: defaultAPIBaseURL,
		Owner:      defaultOwner,
		Repo:       defaultRepo,
		Label:      defaultKREPLabel,
		OutputPath: defaultOutputPath,
		Timeout:    30 * time.Second,
	}

	flag.StringVar(&cfg.APIBaseURL, "api-base-url", cfg.APIBaseURL, "GitHub API base URL")
	flag.StringVar(&cfg.Owner, "owner", cfg.Owner, "GitHub repository owner")
	flag.StringVar(&cfg.Repo, "repo", cfg.Repo, "GitHub repository name")
	flag.StringVar(&cfg.Label, "label", cfg.Label, "Label used to discover KREP pull requests")
	flag.StringVar(&cfg.OutputPath, "out", cfg.OutputPath, "Path to write generated roadmap JSON")
	flag.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "HTTP timeout")
	flag.Parse()

	resolvedOutputPath, err := resolveOutputPath(cfg.OutputPath)
	if err != nil {
		fatalf("resolve output path: %v", err)
	}
	cfg.OutputPath = resolvedOutputPath

	client, err := newGitHubClient(cfg)
	if err != nil {
		fatalf("configure GitHub client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	doc, err := generateRoadmap(ctx, client, cfg)
	if err != nil {
		fatalf("generate roadmap data: %v", err)
	}

	if err := writeOutput(cfg.OutputPath, doc); err != nil {
		fatalf("write output: %v", err)
	}

	fmt.Fprintf(os.Stdout, "wrote %d KREPs to %s\n", len(doc.Items), cfg.OutputPath)
}

func generateRoadmap(ctx context.Context, client githubClient, cfg crawlConfig) (outputDocument, error) {
	milestones, err := client.listMilestones(ctx, cfg.Owner, cfg.Repo)
	if err != nil {
		return outputDocument{}, err
	}

	numbers, err := client.listLabeledPullRequests(ctx, cfg.Owner, cfg.Repo, cfg.Label)
	if err != nil {
		return outputDocument{}, err
	}

	items := make([]roadmapItem, 0, len(numbers))
	for _, number := range numbers {
		pr, err := client.getPullRequest(ctx, cfg.Owner, cfg.Repo, number)
		if err != nil {
			return outputDocument{}, fmt.Errorf("pull request #%d: %w", number, err)
		}

		item, ok := buildRoadmapItem(pr)
		if !ok {
			continue
		}

		items = append(items, item)
	}

	slices.SortFunc(items, func(left, right roadmapItem) int {
		return compareRoadmapIDs(left.ID, right.ID)
	})

	releases := buildReleaseLanes(cfg.Owner, cfg.Repo, milestones)

	return outputDocument{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Repository:    fmt.Sprintf("%s/%s", cfg.Owner, cfg.Repo),
		Label:         cfg.Label,
		Releases:      releases,
		Items:         items,
	}, nil
}

func buildRoadmapItem(pr *githubapi.PullRequest) (roadmapItem, bool) {
	id, title, ok := parseKREPTitle(pr.GetTitle())
	if !ok {
		return roadmapItem{}, false
	}

	release := ""
	var milestone *milestoneMeta
	if pr.Milestone != nil {
		release = normalizeRelease(pr.GetMilestone().GetTitle())
		milestone = &milestoneMeta{
			Number: pr.GetMilestone().GetNumber(),
			Title:  pr.GetMilestone().GetTitle(),
			State:  pr.GetMilestone().GetState(),
			URL:    pr.GetMilestone().GetHTMLURL(),
		}
	}

	merged := pr.MergedAt != nil && !pr.GetMergedAt().IsZero()

	labels := make([]string, 0, len(pr.Labels))
	for _, label := range pr.Labels {
		if label == nil || label.GetName() == "" {
			continue
		}
		labels = append(labels, label.GetName())
	}
	slices.Sort(labels)

	assignees := make([]string, 0, len(pr.Assignees))
	for _, assignee := range pr.Assignees {
		if assignee == nil || assignee.GetLogin() == "" {
			continue
		}
		assignees = append(assignees, assignee.GetLogin())
	}
	slices.Sort(assignees)

	author := ""
	if pr.User != nil {
		author = pr.GetUser().GetLogin()
	}

	mergedAt := ""
	if merged {
		mergedAt = pr.GetMergedAt().Time.Format(time.RFC3339)
	}

	summary := extractSummary(pr.GetBody())
	if summary == "" {
		summary = title
	}

	return roadmapItem{
		ID:         fmt.Sprintf("%03d", id),
		Title:      title,
		Summary:    summary,
		Status:     deriveRoadmapStatus(pr),
		Owners:     uniqueStrings([]string{author}),
		Release:    release,
		ProposalPR: pr.GetNumber(),
		Milestone:  milestone,
		GitHub: githubMeta{
			State:     normalizeGitHubState(pr.GetState(), merged),
			Draft:     pr.GetDraft(),
			Merged:    merged,
			MergedAt:  mergedAt,
			Author:    author,
			Assignees: assignees,
			Labels:    labels,
		},
	}, true
}

func deriveRoadmapStatus(pr *githubapi.PullRequest) string {
	if pr.MergedAt != nil && !pr.GetMergedAt().IsZero() {
		if pr.Milestone != nil && strings.EqualFold(pr.GetMilestone().GetState(), "closed") {
			return "Released"
		}
		if pr.Milestone != nil {
			return "Scheduled"
		}
		return "Accepted"
	}

	if pr.Milestone != nil {
		return "Scheduled"
	}

	if pr.GetDraft() {
		return "Draft"
	}

	return "In Review"
}

func normalizeGitHubState(state string, merged bool) string {
	if merged {
		return "merged"
	}

	return strings.ToLower(strings.TrimSpace(state))
}

func parseKREPTitle(title string) (int, string, bool) {
	matches := krepTitlePattern.FindStringSubmatchIndex(title)
	if matches == nil {
		return 0, "", false
	}

	id, err := strconv.Atoi(title[matches[2]:matches[3]])
	if err != nil {
		return 0, "", false
	}

	cleanTitle := strings.TrimSpace(title[matches[1]:])
	cleanTitle = strings.TrimLeft(cleanTitle, " :-")
	cleanTitle = strings.TrimSpace(cleanTitle)
	if cleanTitle == "" {
		cleanTitle = strings.TrimSpace(title)
	}

	return id, uppercaseFirst(cleanTitle), true
}

func compareRoadmapIDs(left string, right string) int {
	leftValue, leftErr := strconv.Atoi(left)
	rightValue, rightErr := strconv.Atoi(right)
	if leftErr == nil && rightErr == nil {
		return leftValue - rightValue
	}

	return strings.Compare(left, right)
}

func buildReleaseLanes(owner string, repo string, milestones []*githubapi.Milestone) []releaseLane {
	type normalizedMilestone struct {
		release   string
		version   releaseVersion
		milestone *githubapi.Milestone
	}

	normalized := make([]normalizedMilestone, 0, len(milestones))
	for _, milestone := range milestones {
		if milestone == nil {
			continue
		}

		release, version, ok := parseReleaseVersion(milestone.GetTitle())
		if !ok {
			continue
		}

		normalized = append(normalized, normalizedMilestone{
			release:   release,
			version:   version,
			milestone: milestone,
		})
	}

	slices.SortFunc(normalized, func(left, right normalizedMilestone) int {
		return compareReleaseVersions(left.version, right.version)
	})

	nextOpenIndex := -1
	for index, milestone := range normalized {
		if strings.EqualFold(milestone.milestone.GetState(), "open") {
			nextOpenIndex = index
			break
		}
	}

	lanes := make([]releaseLane, 0, len(normalized))
	for index, milestone := range normalized {
		phase := "planned"
		url := milestone.milestone.GetHTMLURL()
		if strings.EqualFold(milestone.milestone.GetState(), "closed") {
			phase = "released"
			url = releaseTagURL(owner, repo, milestone.release)
		} else if index == nextOpenIndex {
			phase = "next"
		}

		lanes = append(lanes, releaseLane{
			Release: milestone.release,
			URL:     url,
			Phase:   phase,
			Milestone: milestoneMeta{
				Number: milestone.milestone.GetNumber(),
				Title:  milestone.milestone.GetTitle(),
				State:  milestone.milestone.GetState(),
				URL:    milestone.milestone.GetHTMLURL(),
			},
		})
	}

	return lanes
}

func normalizeRelease(title string) string {
	release, _, ok := parseReleaseVersion(title)
	if !ok {
		return strings.TrimSpace(title)
	}

	return release
}

type releaseVersion struct {
	major int
	minor int
	patch int
}

func parseReleaseVersion(title string) (string, releaseVersion, bool) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", releaseVersion{}, false
	}

	version := strings.TrimPrefix(trimmed, "v")
	parts := strings.Split(version, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return "", releaseVersion{}, false
	}

	numbers := make([]int, 0, 3)
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return "", releaseVersion{}, false
		}
		numbers = append(numbers, value)
	}

	for len(numbers) < 3 {
		numbers = append(numbers, 0)
	}

	parsed := releaseVersion{
		major: numbers[0],
		minor: numbers[1],
		patch: numbers[2],
	}

	return fmt.Sprintf("v%d.%d.%d", parsed.major, parsed.minor, parsed.patch), parsed, true
}

func compareReleaseVersions(left releaseVersion, right releaseVersion) int {
	if left.major != right.major {
		return left.major - right.major
	}
	if left.minor != right.minor {
		return left.minor - right.minor
	}

	return left.patch - right.patch
}

func releaseTagURL(owner string, repo string, version string) string {
	return fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", owner, repo, version)
}

func extractSummary(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}

	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	paragraphs := strings.Split(normalized, "\n\n")
	for _, paragraph := range paragraphs {
		candidate := strings.TrimSpace(paragraph)
		if candidate == "" {
			continue
		}
		if strings.HasPrefix(candidate, "```") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(candidate), "link for markdown:") {
			continue
		}
		if headingOnly(candidate) {
			continue
		}

		lines := strings.Split(candidate, "\n")
		cleaned := make([]string, 0, len(lines))
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			cleaned = append(cleaned, trimmed)
		}

		summary := strings.Join(cleaned, " ")
		if hasMeaningfulSummaryContent(summary) {
			return summary
		}
	}

	return ""
}

func hasMeaningfulSummaryContent(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSymbol(r) {
			return true
		}
	}

	return false
}

func headingOnly(paragraph string) bool {
	for _, line := range strings.Split(paragraph, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			return false
		}
	}

	return true
}

func uppercaseFirst(value string) string {
	for index, r := range value {
		return string(unicode.ToUpper(r)) + value[index+len(string(r)):]
	}

	return value
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}

	return result
}

func writeOutput(path string, doc outputDocument) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func githubToken() string {
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token
	}
	if token := strings.TrimSpace(os.Getenv("GH_TOKEN")); token != "" {
		return token
	}

	return ""
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func resolveOutputPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		return "", err
	}

	return filepath.Join(repoRoot, filepath.FromSlash(path)), nil
}

func findRepoRoot() (string, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return "", err
	}

	current := workingDirectory
	for {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current, nil
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("could not find repository root")
		}
		current = parent
	}
}

func newGitHubClient(cfg crawlConfig) (githubClient, error) {
	httpClient := &http.Client{Timeout: cfg.Timeout}

	client := githubapi.NewClient(httpClient)
	if cfg.APIBaseURL != defaultAPIBaseURL {
		var err error
		client, err = githubapi.NewEnterpriseClient(
			normalizeAPIBaseURL(cfg.APIBaseURL),
			normalizeAPIBaseURL(cfg.APIBaseURL),
			httpClient,
		)
		if err != nil {
			return githubClient{}, err
		}
	}

	if token := githubToken(); token != "" {
		client = client.WithAuthToken(token)
	}

	client.UserAgent = "kro-krep-crawl"

	return githubClient{client: client}, nil
}

func normalizeAPIBaseURL(rawURL string) string {
	return strings.TrimRight(rawURL, "/") + "/"
}

func (client githubClient) listLabeledPullRequests(
	ctx context.Context,
	owner string,
	repo string,
	label string,
) ([]int, error) {
	var numbers []int
	options := &githubapi.IssueListByRepoOptions{
		State:  "all",
		Labels: []string{label},
		ListOptions: githubapi.ListOptions{
			PerPage: 100,
		},
	}

	for {
		issues, response, err := client.client.Issues.ListByRepo(ctx, owner, repo, options)
		if err != nil {
			return nil, err
		}

		for _, issue := range issues {
			if issue == nil || !issue.IsPullRequest() {
				continue
			}
			numbers = append(numbers, issue.GetNumber())
		}

		if response == nil || response.NextPage == 0 {
			break
		}
		options.ListOptions.Page = response.NextPage
	}

	if len(numbers) == 0 {
		return nil, errors.New("no labeled pull requests found")
	}

	return numbers, nil
}

func (client githubClient) listMilestones(
	ctx context.Context,
	owner string,
	repo string,
) ([]*githubapi.Milestone, error) {
	var milestones []*githubapi.Milestone
	options := &githubapi.MilestoneListOptions{
		State: "all",
		ListOptions: githubapi.ListOptions{
			PerPage: 100,
		},
	}

	for {
		batch, response, err := client.client.Issues.ListMilestones(ctx, owner, repo, options)
		if err != nil {
			return nil, err
		}

		milestones = append(milestones, batch...)

		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}

	return milestones, nil
}

func (client githubClient) getPullRequest(
	ctx context.Context,
	owner string,
	repo string,
	number int,
) (*githubapi.PullRequest, error) {
	pr, _, err := client.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}

	return pr, nil
}
