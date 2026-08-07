package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/viper"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/pkg/models"
)

// RegisterWorkspace records a directory workspace in the global config.
// The path is expanded and resolved; an empty name defaults to the directory
// base name. Re-registering an existing path updates its name; registering an
// existing name for a different path is an error.
func RegisterWorkspace(workspace models.Workspace) (models.Workspace, error) {
	path, err := normalizeProjectPath(workspace.Path)
	if err != nil {
		return models.Workspace{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return models.Workspace{}, fmt.Errorf("workspace path %s: %w", workspace.Path, err)
	}
	if !info.IsDir() {
		return models.Workspace{}, fmt.Errorf("workspace path %s is not a directory", workspace.Path)
	}
	if err := tmux.ValidateStartDirectory(path); err != nil {
		return models.Workspace{}, err
	}
	workspace.Path = path
	if strings.TrimSpace(workspace.Name) == "" {
		workspace.Name = filepath.Base(path)
	}
	workspace.Name = strings.TrimSpace(workspace.Name)

	var workspaces []models.Workspace
	configPath := filepath.Join(getConfigDir(), configName+"."+configType)
	if _, err := (globalConfigStore{path: configPath}).mutate(
		func(globalViper *viper.Viper) (bool, error) {
			if err := globalViper.UnmarshalKey("workspaces", &workspaces); err != nil {
				return false, fmt.Errorf("failed to read workspaces: %w", err)
			}
			updated := false
			for i := range workspaces {
				if sameProjectPath(workspaces[i].Path, workspace.Path) {
					workspaces[i] = workspace
					updated = true
					continue
				}
				if strings.EqualFold(workspaces[i].Name, workspace.Name) {
					return false, fmt.Errorf(
						"workspace name %q is already registered for %s; choose another with --name",
						workspace.Name, workspaces[i].Path,
					)
				}
			}
			if !updated {
				workspaces = append(workspaces, workspace)
			}
			globalViper.Set("workspaces", workspaces)
			return true, nil
		},
	); err != nil {
		return models.Workspace{}, err
	}
	viper.Set("workspaces", workspaces)
	return workspace, nil
}

// UnregisterWorkspace removes a directory workspace from the global config by
// name. The directory itself is never touched.
func UnregisterWorkspace(name string) error {
	name = strings.TrimSpace(name)
	var workspaces []models.Workspace
	configPath := filepath.Join(getConfigDir(), configName+"."+configType)
	if _, err := (globalConfigStore{path: configPath}).mutate(
		func(globalViper *viper.Viper) (bool, error) {
			if err := globalViper.UnmarshalKey("workspaces", &workspaces); err != nil {
				return false, fmt.Errorf("failed to read workspaces: %w", err)
			}
			kept := workspaces[:0]
			removed := false
			for _, workspace := range workspaces {
				if strings.EqualFold(workspace.Name, name) {
					removed = true
					continue
				}
				kept = append(kept, workspace)
			}
			if !removed {
				if len(workspaces) == 0 {
					return false, fmt.Errorf("no workspace named %q; no workspaces registered", name)
				}
				names := make([]string, 0, len(workspaces))
				for _, workspace := range workspaces {
					names = append(names, workspace.Name)
				}
				sort.Strings(names)
				return false, fmt.Errorf("no workspace named %q; registered: %s", name, strings.Join(names, ", "))
			}
			workspaces = kept
			globalViper.Set("workspaces", workspaces)
			return true, nil
		},
	); err != nil {
		return err
	}
	viper.Set("workspaces", workspaces)
	return nil
}
