package cmd

import (
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

type directoryWorkspaceRecord struct {
	Name           string                `json:"name"`
	Path           string                `json:"path"`
	SessionName    string                `json:"session_name"`
	SessionLive    bool                  `json:"session_live"`
	TmuxSocketName string                `json:"tmux_socket_name,omitempty"`
	TmuxAttachMode models.TmuxAttachMode `json:"tmux_attach_mode"`
}

func directoryWorkspaceRecordsFromSessions(
	workspaces []models.Workspace,
	sessions []tmux.WorkspaceSession,
) []directoryWorkspaceRecord {
	records := make([]directoryWorkspaceRecord, 0, len(workspaces))
	for index, workspace := range workspaces {
		if index >= len(sessions) {
			break
		}
		selected := sessions[index]
		records = append(records, directoryWorkspaceRecord{
			Name:           workspace.Name,
			Path:           workspace.Path,
			SessionName:    selected.Endpoint.SessionName,
			SessionLive:    selected.Live,
			TmuxSocketName: selected.Endpoint.SocketName,
			TmuxAttachMode: models.TmuxAttachDirect,
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
