package repositorysnapshot

import (
	"bytes"
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const (
	detectorManifest         = "manifest"
	detectorLockfile         = "lockfile"
	detectorWorkspace        = "workspace"
	detectorDocker           = "docker"
	detectorCI               = "ci-workflow"
	detectorMigration        = "migration-directory"
	detectorAPIContract      = "api-contract"
	detectorConfiguration    = "configuration-file"
	detectorEnvironmentName  = "environment-name"
	detectorCommandCandidate = "command-candidate"
)

var (
	allDetectors = []Detector{
		{ID: detectorAPIContract, Version: DetectorVersion},
		{ID: detectorCI, Version: DetectorVersion},
		{ID: detectorCommandCandidate, Version: DetectorVersion},
		{ID: detectorConfiguration, Version: DetectorVersion},
		{ID: detectorDocker, Version: DetectorVersion},
		{ID: detectorEnvironmentName, Version: DetectorVersion},
		{ID: detectorLockfile, Version: DetectorVersion},
		{ID: detectorManifest, Version: DetectorVersion},
		{ID: detectorMigration, Version: DetectorVersion},
		{ID: detectorWorkspace, Version: DetectorVersion},
	}
	dotenvName      = regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*=`)
	expansionName   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`)
	dockerName      = regexp.MustCompile(`(?mi)^[ \t]*(?:ARG|ENV)[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)
	makeTarget      = regexp.MustCompile(`(?m)^([A-Za-z0-9][A-Za-z0-9._-]{0,63})[ \t]*:(?:[^=]|$)`)
	safeCommandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	commandNames    = []string{"build", "check", "lint", "test", "test:e2e", "test:unit", "typecheck", "verify"}
	manifestNames   = map[string]string{
		"go.mod": "go", "package.json": "node", "Cargo.toml": "rust",
		"pyproject.toml": "python", "requirements.txt": "python", "Pipfile": "python",
		"Gemfile": "ruby", "composer.json": "php", "pom.xml": "maven",
		"build.gradle": "gradle", "build.gradle.kts": "gradle", "settings.gradle": "gradle",
		"settings.gradle.kts": "gradle", "Package.swift": "swift", "mix.exs": "elixir",
		"pubspec.yaml": "dart", "deno.json": "deno", "deno.jsonc": "deno",
	}
	lockNames = map[string]bool{
		"go.sum": true, "package-lock.json": true, "pnpm-lock.yaml": true,
		"yarn.lock": true, "bun.lock": true, "bun.lockb": true, "Cargo.lock": true,
		"poetry.lock": true, "uv.lock": true, "Pipfile.lock": true, "Gemfile.lock": true,
		"composer.lock": true, "mix.lock": true, "pubspec.lock": true,
		"gradle.lockfile": true, "Package.resolved": true,
	}
	workspaceNames = map[string]bool{
		"go.work": true, "pnpm-workspace.yaml": true, "lerna.json": true,
		"nx.json": true, "turbo.json": true, "rush.json": true,
	}
)

func detectTree(entries []treeEntry) (Response, []treeEntry) {
	response := Response{
		Detectors:    append([]Detector(nil), allDetectors...),
		Observations: []Observation{}, Limitations: []Limitation{},
	}
	selected := make([]treeEntry, 0)
	migrationDirs := make(map[string]struct{})
	for _, entry := range entries {
		if entry.objectType == "commit" || entry.mode == "160000" {
			addLimitation(&response, Limitation{Code: "gitlink_not_inspected", EvidencePath: entry.path})
			continue
		}
		if entry.objectType != "blob" {
			continue
		}
		base := path.Base(entry.path)
		lowerBase := strings.ToLower(base)
		lowerPath := strings.ToLower(entry.path)
		if kind, ok := manifestKind(base); ok {
			addObservation(&response, detectorManifest, entry.path, kind)
		}
		if lockNames[base] {
			addObservation(&response, detectorLockfile, entry.path, "lockfile")
		}
		if workspaceNames[base] {
			addObservation(&response, detectorWorkspace, entry.path, "workspace-definition")
		}
		if isDockerFile(lowerBase) {
			addObservation(&response, detectorDocker, entry.path, "dockerfile")
		}
		if isComposeFile(lowerBase) {
			addObservation(&response, detectorDocker, entry.path, "compose-file")
		}
		if isCIFile(lowerPath, base) {
			addObservation(&response, detectorCI, entry.path, "workflow-file")
		}
		if isAPIContract(lowerBase) {
			addObservation(&response, detectorAPIContract, entry.path, "api-contract")
		}
		if isConfigurationFile(lowerBase) {
			addObservation(&response, detectorConfiguration, entry.path, "configuration-file")
		}
		for _, directory := range migrationDirectories(entry.path) {
			migrationDirs[directory] = struct{}{}
		}
		if shouldInspectBlob(base, lowerBase) {
			selected = append(selected, entry)
		}
	}
	directories := make([]string, 0, len(migrationDirs))
	for directory := range migrationDirs {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	for _, directory := range directories {
		addObservation(&response, detectorMigration, directory, "migration-directory")
	}
	slices.SortFunc(selected, func(left, right treeEntry) int { return strings.Compare(left.path, right.path) })
	return response, selected
}

func detectBlob(evidencePath string, contents []byte, response *Response) {
	base := path.Base(evidencePath)
	lowerBase := strings.ToLower(base)
	if lowerBase == "package.json" {
		detectPackageJSON(evidencePath, contents, response)
	}
	if base == "Makefile" || strings.HasSuffix(base, ".mk") {
		detectMakeTargets(evidencePath, contents, response)
	}
	if isExampleEnvironmentFile(lowerBase) {
		addEnvironmentMatches(evidencePath, contents, dotenvName, response)
	}
	if isDockerFile(lowerBase) {
		addEnvironmentMatches(evidencePath, contents, dockerName, response)
	}
	if isComposeFile(lowerBase) || isConfigurationFile(lowerBase) {
		addEnvironmentMatches(evidencePath, contents, expansionName, response)
	}
}

func detectPackageJSON(evidencePath string, contents []byte, response *Response) {
	var document struct {
		Scripts    map[string]json.RawMessage `json:"scripts"`
		Workspaces json.RawMessage            `json:"workspaces"`
	}
	if err := json.Unmarshal(contents, &document); err != nil {
		addLimitation(response, Limitation{Code: "invalid_selected_metadata", EvidencePath: evidencePath})
		return
	}
	if len(document.Workspaces) > 0 && !bytes.Equal(bytes.TrimSpace(document.Workspaces), []byte("null")) {
		addObservation(response, detectorWorkspace, evidencePath, "package-workspaces")
	}
	runner := "npm"
	for _, observation := range response.Observations {
		switch path.Base(observation.EvidencePath) {
		case "pnpm-lock.yaml":
			runner = "pnpm"
		case "yarn.lock":
			if runner == "npm" {
				runner = "yarn"
			}
		case "bun.lock", "bun.lockb":
			runner = "bun"
		}
	}
	for _, name := range commandNames {
		if _, ok := document.Scripts[name]; ok && safeCommandName.MatchString(name) {
			addObservation(response, detectorCommandCandidate, evidencePath, runner+" run "+name)
		}
	}
}

func detectMakeTargets(evidencePath string, contents []byte, response *Response) {
	allowed := make(map[string]bool, len(commandNames))
	for _, name := range commandNames {
		allowed[name] = true
	}
	for _, match := range makeTarget.FindAllSubmatch(contents, -1) {
		name := string(match[1])
		if allowed[name] {
			addObservation(response, detectorCommandCandidate, evidencePath, "make "+name)
		}
	}
}

func addEnvironmentMatches(evidencePath string, contents []byte, pattern *regexp.Regexp, response *Response) {
	names := make(map[string]struct{})
	for _, match := range pattern.FindAllSubmatch(contents, -1) {
		if len(match) > 1 {
			names[string(match[1])] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		addObservation(response, detectorEnvironmentName, evidencePath, name)
	}
}

func finalizeResponse(response *Response) {
	addFixedCommandCandidates(response)
	slices.SortFunc(response.Observations, func(left, right Observation) int {
		if result := strings.Compare(left.DetectorID, right.DetectorID); result != 0 {
			return result
		}
		if result := strings.Compare(left.EvidencePath, right.EvidencePath); result != 0 {
			return result
		}
		return strings.Compare(left.Observation, right.Observation)
	})
	slices.SortFunc(response.Limitations, func(left, right Limitation) int {
		if result := strings.Compare(left.Code, right.Code); result != 0 {
			return result
		}
		return strings.Compare(left.EvidencePath, right.EvidencePath)
	})
}

func addFixedCommandCandidates(response *Response) {
	files := make(map[string]string)
	for _, observation := range response.Observations {
		files[path.Base(observation.EvidencePath)] = observation.EvidencePath
	}
	manifestObservations := append([]Observation(nil), response.Observations...)
	for _, manifest := range manifestObservations {
		if manifest.DetectorID != detectorManifest {
			continue
		}
		for _, candidate := range []struct {
			manifest string
			command  string
		}{
			{"go", "go build ./..."}, {"go", "go test ./..."},
			{"rust", "cargo build"}, {"rust", "cargo test"},
			{"python", "python -m pytest"}, {"maven", "mvn test"},
			{"gradle", "./gradlew test"}, {"ruby", "bundle exec rake test"},
		} {
			if manifest.Observation != candidate.manifest {
				continue
			}
			if candidate.manifest == "gradle" && files["gradlew"] == "" {
				continue
			}
			addObservation(response, detectorCommandCandidate, manifest.EvidencePath, candidate.command)
		}
	}
}

func addObservation(response *Response, detectorID, evidencePath, observation string) {
	if len(response.Observations) >= MaxObservations {
		addLimitation(response, Limitation{Code: "observation_limit"})
		return
	}
	response.Observations = append(response.Observations, Observation{
		DetectorID: detectorID, DetectorVersion: DetectorVersion,
		EvidencePath: evidencePath, Observation: observation,
	})
}

func addLimitation(response *Response, value Limitation) {
	for _, existing := range response.Limitations {
		if existing == value {
			return
		}
	}
	if len(response.Limitations) >= MaxLimitations {
		return
	}
	response.Limitations = append(response.Limitations, value)
}

func manifestKind(base string) (string, bool) {
	if kind, ok := manifestNames[base]; ok {
		return kind, true
	}
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, ".csproj") || strings.HasSuffix(lower, ".sln") {
		return "dotnet", true
	}
	return "", false
}

func isDockerFile(lowerBase string) bool {
	return lowerBase == "dockerfile" || strings.HasPrefix(lowerBase, "dockerfile.") ||
		strings.HasSuffix(lowerBase, ".dockerfile")
}

func isComposeFile(lowerBase string) bool {
	return lowerBase == "compose.yaml" || lowerBase == "compose.yml" ||
		lowerBase == "docker-compose.yaml" || lowerBase == "docker-compose.yml" ||
		strings.HasPrefix(lowerBase, "compose.") && (strings.HasSuffix(lowerBase, ".yaml") || strings.HasSuffix(lowerBase, ".yml"))
}

func isCIFile(lowerPath, base string) bool {
	return strings.HasPrefix(lowerPath, ".github/workflows/") &&
		(strings.HasSuffix(lowerPath, ".yml") || strings.HasSuffix(lowerPath, ".yaml")) ||
		lowerPath == ".gitlab-ci.yml" || base == "Jenkinsfile" ||
		lowerPath == ".circleci/config.yml" || lowerPath == ".circleci/config.yaml" ||
		lowerPath == "azure-pipelines.yml" || lowerPath == ".buildkite/pipeline.yml"
}

func isAPIContract(lowerBase string) bool {
	return strings.HasSuffix(lowerBase, ".proto") || strings.HasSuffix(lowerBase, ".graphql") ||
		strings.HasSuffix(lowerBase, ".gql") || strings.HasPrefix(lowerBase, "openapi.") ||
		strings.HasPrefix(lowerBase, "swagger.") || strings.HasPrefix(lowerBase, "asyncapi.")
}

func isConfigurationFile(lowerBase string) bool {
	if isExampleEnvironmentFile(lowerBase) {
		return true
	}
	return lowerBase == "config.yaml" || lowerBase == "config.yml" ||
		lowerBase == "config.json" || lowerBase == "application.yaml" ||
		lowerBase == "application.yml" || lowerBase == "appsettings.json" ||
		strings.Contains(lowerBase, ".example.") || strings.Contains(lowerBase, ".sample.") ||
		strings.Contains(lowerBase, ".template.")
}

func isExampleEnvironmentFile(lowerBase string) bool {
	return strings.HasPrefix(lowerBase, ".env.") &&
		(strings.Contains(lowerBase, "example") || strings.Contains(lowerBase, "sample") || strings.Contains(lowerBase, "template")) ||
		strings.HasSuffix(lowerBase, ".example.env") || strings.HasSuffix(lowerBase, ".sample.env")
}

func shouldInspectBlob(base, lowerBase string) bool {
	return lowerBase == "package.json" || base == "Makefile" || strings.HasSuffix(base, ".mk") ||
		isExampleEnvironmentFile(lowerBase) || isDockerFile(lowerBase) ||
		isComposeFile(lowerBase) || isConfigurationFile(lowerBase)
}

func migrationDirectories(filePath string) []string {
	parts := strings.Split(filePath, "/")
	result := make([]string, 0, 1)
	for index, part := range parts[:len(parts)-1] {
		lower := strings.ToLower(part)
		if lower == "migrations" || lower == "migration" || lower == "migrate" {
			result = append(result, strings.Join(parts[:index+1], "/"))
		}
	}
	return result
}
