package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	os.WriteFile(path, []byte(`[identity]
	name = Vee
	email = vee@example.com
[embedding]
	url = http://localhost:9999
	model = nomic-embed-text
`), 0600)

	m, err := parseConfig(path, nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if got := lastValue(m, "identity.name"); got != "Vee" {
		t.Errorf("identity.name = %q, want %q", got, "Vee")
	}
	if got := lastValue(m, "identity.email"); got != "vee@example.com" {
		t.Errorf("identity.email = %q, want %q", got, "vee@example.com")
	}
	if got := lastValue(m, "embedding.url"); got != "http://localhost:9999" {
		t.Errorf("embedding.url = %q, want %q", got, "http://localhost:9999")
	}
}

func TestParseConfigLastWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	os.WriteFile(path, []byte(`[identity]
	name = First
[identity]
	name = Second
`), 0600)

	m, err := parseConfig(path, nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if got := lastValue(m, "identity.name"); got != "Second" {
		t.Errorf("identity.name = %q, want %q", got, "Second")
	}
}

func TestParseConfigMultiValued(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	os.WriteFile(path, []byte(`[ephemeral]
	mount = ~/.claude:/root/.claude
	mount = ~/.claude.json:/root/.claude.json:rw
	env = FOO=bar
	env = BAZ=qux
`), 0600)

	m, err := parseConfig(path, nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	mounts := m["ephemeral.mount"]
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}
	if mounts[0] != "~/.claude:/root/.claude" {
		t.Errorf("mount[0] = %q", mounts[0])
	}
	if mounts[1] != "~/.claude.json:/root/.claude.json:rw" {
		t.Errorf("mount[1] = %q", mounts[1])
	}

	envs := m["ephemeral.env"]
	if len(envs) != 2 {
		t.Fatalf("expected 2 envs, got %d", len(envs))
	}
}

func TestParseConfigInclude(t *testing.T) {
	dir := t.TempDir()

	// Write the included file
	incPath := filepath.Join(dir, "extra.conf")
	os.WriteFile(incPath, []byte(`[identity]
	name = Included
	email = inc@example.com
`), 0600)

	// Write the main config with an include
	mainPath := filepath.Join(dir, "config")
	os.WriteFile(mainPath, []byte(`[include]
	path = extra.conf
[embedding]
	url = http://localhost:5000
`), 0600)

	m, err := parseConfig(mainPath, nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	// Values from the included file should be present
	if got := lastValue(m, "identity.name"); got != "Included" {
		t.Errorf("identity.name = %q, want %q", got, "Included")
	}
	if got := lastValue(m, "embedding.url"); got != "http://localhost:5000" {
		t.Errorf("embedding.url = %q, want %q", got, "http://localhost:5000")
	}
}

func TestParseConfigIncludeOrdering(t *testing.T) {
	dir := t.TempDir()

	// Included file sets name = "FromInclude"
	incPath := filepath.Join(dir, "inc.conf")
	os.WriteFile(incPath, []byte(`[identity]
	name = FromInclude
`), 0600)

	// Main file: set name before include, then override after
	mainPath := filepath.Join(dir, "config")
	os.WriteFile(mainPath, []byte(`[identity]
	name = Before
[include]
	path = inc.conf
[identity]
	name = After
`), 0600)

	m, err := parseConfig(mainPath, nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	// "After" was set last, so it should win
	if got := lastValue(m, "identity.name"); got != "After" {
		t.Errorf("identity.name = %q, want %q", got, "After")
	}
}

func TestParseConfigIncludeIfGitdirMatch(t *testing.T) {
	dir := t.TempDir()

	out, err := exec.Command("git", "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Skipf("not inside a git repo: %v", err)
	}
	gitDir := filepath.Dir(strings.TrimRight(string(out), "\n"))

	// Write included file
	incPath := filepath.Join(dir, "work.conf")
	os.WriteFile(incPath, []byte(`[identity]
	name = WorkBot
`), 0600)

	// Use the git root with trailing "/" for prefix match
	mainPath := filepath.Join(dir, "config")
	os.WriteFile(mainPath, []byte(`[includeIf "gitdir:`+gitDir+`/"]
	path = `+incPath+`
`), 0600)

	m, err := parseConfig(mainPath, nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if got := lastValue(m, "identity.name"); got != "WorkBot" {
		t.Errorf("identity.name = %q, want %q", got, "WorkBot")
	}
}

func TestParseConfigIncludeIfGitdirNoTrailingSlash(t *testing.T) {
	dir := t.TempDir()

	out, err := exec.Command("git", "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Skipf("not inside a git repo: %v", err)
	}
	gitDir := strings.TrimRight(string(out), "\n")

	// Write included file
	incPath := filepath.Join(dir, "work.conf")
	os.WriteFile(incPath, []byte(`[identity]
	name = ExactBot
`), 0600)

	// Exact match (no trailing /) against the .git directory itself
	mainPath := filepath.Join(dir, "config")
	os.WriteFile(mainPath, []byte(`[includeIf "gitdir:`+gitDir+`"]
	path = `+incPath+`
`), 0600)

	m, err := parseConfig(mainPath, nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if got := lastValue(m, "identity.name"); got != "ExactBot" {
		t.Errorf("identity.name = %q, want %q", got, "ExactBot")
	}
}

func TestParseConfigIncludeIfGitdirNoMatch(t *testing.T) {
	dir := t.TempDir()

	incPath := filepath.Join(dir, "work.conf")
	os.WriteFile(incPath, []byte(`[identity]
	name = WorkBot
`), 0600)

	mainPath := filepath.Join(dir, "config")
	os.WriteFile(mainPath, []byte(`[includeIf "gitdir:/nonexistent/path/"]
	path = `+incPath+`
`), 0600)

	m, err := parseConfig(mainPath, nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if got := lastValue(m, "identity.name"); got != "" {
		t.Errorf("identity.name = %q, want empty (should not include)", got)
	}
}

func TestParseConfigMissingInclude(t *testing.T) {
	dir := t.TempDir()

	mainPath := filepath.Join(dir, "config")
	os.WriteFile(mainPath, []byte(`[include]
	path = /nonexistent/file.conf
[identity]
	name = StillHere
`), 0600)

	m, err := parseConfig(mainPath, nil)
	if err != nil {
		t.Fatalf("parseConfig should not error on missing include: %v", err)
	}

	if got := lastValue(m, "identity.name"); got != "StillHere" {
		t.Errorf("identity.name = %q, want %q", got, "StillHere")
	}
}

func TestParseMountSpec(t *testing.T) {
	tests := []struct {
		input      string
		wantSource string
		wantTarget string
		wantMode   string
		wantErr    bool
	}{
		{"~/.claude:/root/.claude", "~/.claude", "/root/.claude", "", false},
		{"~/.claude.json:/root/.claude.json:rw", "~/.claude.json", "/root/.claude.json", "rw", false},
		{"/src:/dst:ro", "/src", "/dst", "ro", false},
		{"invalid", "", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ms, err := parseMountSpec(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ms.Source != tt.wantSource {
				t.Errorf("Source = %q, want %q", ms.Source, tt.wantSource)
			}
			if ms.Target != tt.wantTarget {
				t.Errorf("Target = %q, want %q", ms.Target, tt.wantTarget)
			}
			if ms.Mount != tt.wantMode {
				t.Errorf("Mount = %q, want %q", ms.Mount, tt.wantMode)
			}
		})
	}
}

func TestHydrateProjectConfig(t *testing.T) {
	m := map[string][]string{
		"ephemeral.dockerfile": {"Dockerfile.dev"},
		"ephemeral.mount":      {"~/.claude:/root/.claude", "~/.claude.json:/root/.claude.json:rw"},
		"ephemeral.env":        {"FOO=bar"},
		"identity.disable":     {"true"},
	}

	cfg := hydrateProjectConfig(m)
	if cfg.Ephemeral == nil {
		t.Fatal("expected non-nil Ephemeral")
	}
	if cfg.Ephemeral.Dockerfile != "Dockerfile.dev" {
		t.Errorf("Dockerfile = %q", cfg.Ephemeral.Dockerfile)
	}
	if len(cfg.Ephemeral.Mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(cfg.Ephemeral.Mounts))
	}
	if cfg.Ephemeral.Mounts[1].Mount != "rw" {
		t.Errorf("mount[1].Mount = %q, want %q", cfg.Ephemeral.Mounts[1].Mount, "rw")
	}
	if cfg.Identity == nil {
		t.Fatal("expected non-nil Identity")
	}
	if !cfg.Identity.Disable {
		t.Error("expected Identity.Disable = true")
	}
}

func TestHydrateProjectConfigCompose(t *testing.T) {
	m := map[string][]string{
		"ephemeral.dockerfile": {"Dockerfile"},
		"ephemeral.compose":    {"docker-compose.yml"},
	}

	cfg := hydrateProjectConfig(m)
	if cfg.Ephemeral == nil {
		t.Fatal("expected non-nil Ephemeral")
	}
	if cfg.Ephemeral.Compose != "docker-compose.yml" {
		t.Errorf("Compose = %q, want %q", cfg.Ephemeral.Compose, "docker-compose.yml")
	}
	if cfg.Ephemeral.Dockerfile != "Dockerfile" {
		t.Errorf("Dockerfile = %q, want %q", cfg.Ephemeral.Dockerfile, "Dockerfile")
	}
}

func TestHydrateProjectConfigComposeOnly(t *testing.T) {
	m := map[string][]string{
		"ephemeral.compose": {"compose.yml"},
	}

	cfg := hydrateProjectConfig(m)
	if cfg.Ephemeral == nil {
		t.Fatal("expected non-nil Ephemeral")
	}
	if cfg.Ephemeral.Compose != "compose.yml" {
		t.Errorf("Compose = %q, want %q", cfg.Ephemeral.Compose, "compose.yml")
	}
}

func TestHydrateUserConfig(t *testing.T) {
	m := map[string][]string{
		"embedding.model":      {"mxbai-embed-large"},
		"embedding.threshold":  {"0.5"},
		"embedding.maxresults": {"20"},
		"identity.name":        {"Vee"},
		"identity.email":       {"vee@example.com"},
	}

	cfg := hydrateUserConfig(m)
	if cfg.Embedding.URL != "http://localhost:11434" {
		t.Errorf("Embedding.URL = %q, want default", cfg.Embedding.URL)
	}
	if cfg.Embedding.Model != "mxbai-embed-large" {
		t.Errorf("Embedding.Model = %q, want %q", cfg.Embedding.Model, "mxbai-embed-large")
	}
	if cfg.Embedding.Threshold != 0.5 {
		t.Errorf("Embedding.Threshold = %f, want 0.5", cfg.Embedding.Threshold)
	}
	if cfg.Embedding.MaxResults != 20 {
		t.Errorf("Embedding.MaxResults = %d, want 20", cfg.Embedding.MaxResults)
	}
	if cfg.Identity == nil || cfg.Identity.Name != "Vee" {
		t.Errorf("Identity.Name = %v", cfg.Identity)
	}
}

func TestHydrateUserConfigDefaults(t *testing.T) {
	cfg := hydrateUserConfig(nil)
	if cfg.Embedding.URL != "http://localhost:11434" {
		t.Errorf("default URL = %q", cfg.Embedding.URL)
	}
	if cfg.Embedding.Model != "nomic-embed-text" {
		t.Errorf("default Model = %q", cfg.Embedding.Model)
	}
	if cfg.Embedding.Threshold != 0.3 {
		t.Errorf("default Threshold = %f", cfg.Embedding.Threshold)
	}
	if cfg.Embedding.MaxResults != 10 {
		t.Errorf("default MaxResults = %d", cfg.Embedding.MaxResults)
	}
	if cfg.Embedding.DupThreshold != 0.85 {
		t.Errorf("default DupThreshold = %f", cfg.Embedding.DupThreshold)
	}
}

func TestResolveIdentity(t *testing.T) {
	tests := []struct {
		name     string
		user     *IdentityConfig
		project  *IdentityConfig
		wantNil  bool
		wantName string
		wantMail string
	}{
		{
			name:    "both nil",
			user:    nil,
			project: nil,
			wantNil: true,
		},
		{
			name:     "user only",
			user:     &IdentityConfig{Name: "Vee", Email: "vee@example.com"},
			project:  nil,
			wantName: "Vee",
			wantMail: "vee@example.com",
		},
		{
			name:     "project only",
			user:     nil,
			project:  &IdentityConfig{Name: "Bot", Email: "bot@proj.dev"},
			wantName: "Bot",
			wantMail: "bot@proj.dev",
		},
		{
			name:    "project disable",
			user:    &IdentityConfig{Name: "Vee", Email: "vee@example.com"},
			project: &IdentityConfig{Disable: true},
			wantNil: true,
		},
		{
			name:     "field-level merge — project overrides email",
			user:     &IdentityConfig{Name: "Vee", Email: "vee@example.com"},
			project:  &IdentityConfig{Email: "bot@proj.dev"},
			wantName: "Vee",
			wantMail: "bot@proj.dev",
		},
		{
			name:     "field-level merge — project overrides name",
			user:     &IdentityConfig{Name: "Vee", Email: "vee@example.com"},
			project:  &IdentityConfig{Name: "ProjectBot"},
			wantName: "ProjectBot",
			wantMail: "vee@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveIdentity(tt.user, tt.project)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil result")
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.Email != tt.wantMail {
				t.Errorf("Email = %q, want %q", got.Email, tt.wantMail)
			}
		})
	}
}

func TestIdentityRule(t *testing.T) {
	t.Run("nil returns empty", func(t *testing.T) {
		got := identityRule(nil)
		if got != "" {
			t.Fatalf("expected empty string, got %q", got)
		}
	})

	t.Run("configured returns rule", func(t *testing.T) {
		cfg := &IdentityConfig{Name: "Vee", Email: "vee@example.com"}
		got := identityRule(cfg)

		want := `<rule object="Identity">
Your name is Vee.
ALWAYS use ` + "`git commit`" + ` with ` + "`" + `--author "Vee <vee@example.com>"` + "`" + `.
</rule>`

		if got != want {
			t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
		}
	})
}

func TestValidateIdentity(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *IdentityConfig
		wantErr bool
	}{
		{name: "nil is ok", cfg: nil, wantErr: false},
		{name: "disabled is ok", cfg: &IdentityConfig{Disable: true}, wantErr: false},
		{name: "complete is ok", cfg: &IdentityConfig{Name: "Vee", Email: "v@e.com"}, wantErr: false},
		{name: "missing name", cfg: &IdentityConfig{Email: "v@e.com"}, wantErr: true},
		{name: "missing email", cfg: &IdentityConfig{Name: "Vee"}, wantErr: true},
		{name: "both missing", cfg: &IdentityConfig{}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIdentity(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateIdentity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMatchPath(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		// Exact match
		{"/home/user/project/.git", "/home/user/project/.git", true},
		{"/home/user/project/.git", "/home/user/other/.git", false},

		// Single * matches one component segment
		{"/home/*/project/.git", "/home/user/project/.git", true},
		{"/home/*/project/.git", "/home/user/other/.git", false},

		// ** matches zero or more components
		{"/**/project/.git", "/home/user/project/.git", true},
		{"/**/project/.git", "/project/.git", true},
		{"/**/.git", "/home/user/project/.git", true},
		{"/home/**/.git", "/home/user/project/.git", true},
		{"/home/**/.git", "/home/.git", true},

		// Trailing /** (from trailing / normalization)
		{"/home/user/**", "/home/user/project/.git", true},
		{"/home/user/**", "/home/user/.git", true},
		{"/home/user/**", "/home/other/.git", false},

		// Relative patterns (prepended with **/)
		{"**/project/.git", "/home/user/project/.git", true},
		{"**/work/**", "/home/user/work/project/.git", true},
		{"**/work/**", "/home/user/personal/project/.git", false},

		// No match
		{"/a/b/c", "/x/y/z", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_vs_"+tt.name, func(t *testing.T) {
			got := matchPath(tt.pattern, tt.name)
			if got != tt.want {
				t.Errorf("matchPath(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
			}
		})
	}
}
