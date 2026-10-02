package config_test

import (
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestMigrateVirtualFolders(t *testing.T) {
	t.Parallel()
	cfg := config.Config{CustomFolders: map[string]config.CustomFolders{
		"Recently Added": {Filters: map[string]string{"last_added": "7d"}},
		"4K":             {Filters: map[string]string{"exclude": "sample", "include": "2160p"}},
	}}

	cfg.MigrateVirtualFolders()

	if cfg.CustomFolders != nil {
		t.Fatal("legacy custom folders were not cleared")
	}
	if len(cfg.VirtualFolders) != 2 {
		t.Fatalf("got %d virtual folders, want 2", len(cfg.VirtualFolders))
	}
	if cfg.VirtualFolders[0].Name != "4K" {
		t.Fatalf("migration order is not deterministic: first folder is %q", cfg.VirtualFolders[0].Name)
	}
	conditions := cfg.VirtualFolders[0].Conditions
	if len(conditions) != 2 || conditions[0].Operator != config.VirtualFolderOperatorContains ||
		conditions[1].Operator != config.VirtualFolderOperatorNotContains {
		t.Fatalf("unexpected migrated conditions: %#v", conditions)
	}
}

func TestMigrateAdvertisedLegacyNameAndCategoryFilters(t *testing.T) {
	t.Parallel()
	cfg := config.Config{CustomFolders: map[string]config.CustomFolders{
		"Movies": {Filters: map[string]string{"name": "*movie?*", "category": "radarr"}},
	}}
	cfg.MigrateVirtualFolders()
	conditions := cfg.VirtualFolders[0].Conditions
	if len(conditions) != 2 {
		t.Fatalf("got %d conditions, want 2", len(conditions))
	}
	if conditions[0].Field != config.VirtualFolderFieldEntryName ||
		conditions[0].Operator != config.VirtualFolderOperatorMatchesRegex {
		t.Fatalf("legacy name wildcard was not migrated to a name regex: %#v", conditions[0])
	}
	if conditions[1].Field != config.VirtualFolderFieldCategory ||
		conditions[1].Operator != config.VirtualFolderOperatorContains {
		t.Fatalf("legacy category was not migrated: %#v", conditions[1])
	}
}

func TestValidateVirtualFoldersRejectsCollisions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		folders []config.VirtualFolder
		debrids []config.Debrid
		want    string
	}{
		{name: "built in", folders: []config.VirtualFolder{{Name: "__ALL__"}}, want: "conflicts"},
		{
			name:    "provider",
			folders: []config.VirtualFolder{{Name: "RealDebrid"}},
			debrids: []config.Debrid{{Name: "realdebrid"}},
			want:    "conflicts",
		},
		{
			name:    "duplicate",
			folders: []config.VirtualFolder{{Name: "Movies"}, {Name: "movies"}},
			want:    "more than once",
		},
		{name: "path separator", folders: []config.VirtualFolder{{Name: "TV/Shows"}}, want: "not portable"},
		{name: "windows reserved", folders: []config.VirtualFolder{{Name: "CON"}}, want: "reserved"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Config{VirtualFolders: tt.folders, Debrids: tt.debrids}
			err := cfg.ValidateVirtualFolders()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateVirtualFolders() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateVirtualFolderConditions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		condition config.VirtualFolderCondition
		wantError string
	}{
		{
			name: "regex",
			condition: config.VirtualFolderCondition{
				Field:    config.VirtualFolderFieldEntryName,
				Operator: config.VirtualFolderOperatorMatchesRegex,
				Value:    "[",
			},
			wantError: "regular expression",
		},
		{
			name: "size",
			condition: config.VirtualFolderCondition{
				Field:    config.VirtualFolderFieldSize,
				Operator: config.VirtualFolderOperatorGreaterThan,
				Value:    "huge",
			},
			wantError: "invalid size",
		},
		{
			name: "duration",
			condition: config.VirtualFolderCondition{
				Field:    config.VirtualFolderFieldAdded,
				Operator: config.VirtualFolderOperatorWithinLast,
				Value:    "soon",
			},
			wantError: "invalid duration",
		},
		{
			name: "count",
			condition: config.VirtualFolderCondition{
				Field:    config.VirtualFolderFieldFileCount,
				Operator: config.VirtualFolderOperatorGreaterThan,
				Value:    "1.5",
			},
			wantError: "whole number",
		},
		{
			name: "field operator",
			condition: config.VirtualFolderCondition{
				Field:    config.VirtualFolderFieldSize,
				Operator: config.VirtualFolderOperatorContains,
				Value:    "1GB",
			},
			wantError: "not valid for size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			folder := config.VirtualFolder{
				Name:       "Test",
				Match:      config.VirtualFolderMatchAll,
				Conditions: []config.VirtualFolderCondition{tt.condition},
			}
			err := config.ValidateVirtualFolder(folder, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ValidateVirtualFolder() error = %v, want containing %q", err, tt.wantError)
			}
		})
	}
}

func TestValidateVirtualFolderAllowsEmptyConditions(t *testing.T) {
	t.Parallel()
	err := config.ValidateVirtualFolder(
		config.VirtualFolder{Name: "Everything", Match: config.VirtualFolderMatchAll},
		nil,
	)
	if err != nil {
		t.Fatalf("empty conditions should be a valid all-items view: %v", err)
	}
}

func TestVirtualFoldersAreRuntimeApplicable(t *testing.T) {
	t.Parallel()
	current := config.Config{VirtualFolders: []config.VirtualFolder{{Name: "Old"}}}
	next := config.Config{VirtualFolders: []config.VirtualFolder{{Name: "New"}}}
	if current.RequiresRestart(&next) {
		t.Fatal("changing only virtual folders should not require a service restart")
	}
}
