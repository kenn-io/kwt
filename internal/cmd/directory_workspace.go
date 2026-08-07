package cmd

import (
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

type directoryWorkspaceRecord struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	SessionName string `json:"session_name"`
	SessionLive bool   `json:"session_live"`
}

func directoryWorkspaceRecords(
	workspaces []models.Workspace,
	sessions []string,
) []directoryWorkspaceRecord {
	records := make([]directoryWorkspaceRecord, 0, len(workspaces))
	for _, workspace := range workspaces {
		sessionName := tmux.DirWorkspaceSessionName(
			workspace.Name,
			workspace.Path,
		)
		sessionLive := false
		if live, ok := tmux.MatchDirWorkspaceSession(
			sessions,
			workspace.Path,
		); ok {
			sessionName = live
			sessionLive = true
		}
		records = append(records, directoryWorkspaceRecord{
			Name:        workspace.Name,
			Path:        workspace.Path,
			SessionName: sessionName,
			SessionLive: sessionLive,
		})
	}
	return records
}

func findRegisteredDirectoryWorkspace(
	workspaces []models.Workspace,
	path string,
) (models.Workspace, bool) {
	pathKey := utils.PathKey(path)
	for _, workspace := range workspaces {
		if utils.PathKey(workspace.Path) == pathKey {
			return workspace, true
		}
	}
	return models.Workspace{}, false
}
