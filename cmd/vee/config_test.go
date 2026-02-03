package main

import (
	"testing"
)

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
