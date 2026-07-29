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

	workspaces, globalViper, err := readGlobalWorkspaces()
	if err != nil {
		return models.Workspace{}, err
	}

	updated := false
	for i := range workspaces {
		if sameProjectPath(workspaces[i].Path, workspace.Path) {
			workspaces[i] = workspace
			updated = true
			continue
		}
		if strings.EqualFold(workspaces[i].Name, workspace.Name) {
			return models.Workspace{}, fmt.Errorf(
				"workspace name %q is already registered for %s; choose another with --name",
				workspace.Name, workspaces[i].Path,
			)
		}
	}
	if !updated {
		workspaces = append(workspaces, workspace)
	}
	if err := writeGlobalWorkspaces(globalViper, workspaces); err != nil {
		return models.Workspace{}, err
	}
	return workspace, nil
}

// UnregisterWorkspace removes a directory workspace from the global config by
// name. The directory itself is never touched.
func UnregisterWorkspace(name string) error {
	name = strings.TrimSpace(name)
	workspaces, globalViper, err := readGlobalWorkspaces()
	if err != nil {
		return err
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
			return fmt.Errorf("no workspace named %q; no workspaces registered", name)
		}
		names := make([]string, 0, len(workspaces))
		for _, workspace := range workspaces {
			names = append(names, workspace.Name)
		}
		sort.Strings(names)
		return fmt.Errorf("no workspace named %q; registered: %s", name, strings.Join(names, ", "))
	}
	return writeGlobalWorkspaces(globalViper, kept)
}

func readGlobalWorkspaces() ([]models.Workspace, *viper.Viper, error) {
	configDir := getConfigDir()
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return nil, nil, fmt.Errorf("failed to create config directory: %w", err)
	}
	globalViper := viper.New()
	globalViper.SetConfigName(configName)
	globalViper.SetConfigType(configType)
	globalViper.AddConfigPath(configDir)
	if err := globalViper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, nil, fmt.Errorf("failed to read config: %w", err)
		}
	}
	var workspaces []models.Workspace
	if err := globalViper.UnmarshalKey("workspaces", &workspaces); err != nil {
		return nil, nil, fmt.Errorf("failed to read workspaces: %w", err)
	}
	return workspaces, globalViper, nil
}

func writeGlobalWorkspaces(globalViper *viper.Viper, workspaces []models.Workspace) error {
	globalViper.Set("workspaces", workspaces)
	configPath := filepath.Join(getConfigDir(), configName+"."+configType)
	if err := globalViper.WriteConfigAs(configPath); err != nil {
		return err
	}
	viper.Set("workspaces", workspaces)
	return nil
}
