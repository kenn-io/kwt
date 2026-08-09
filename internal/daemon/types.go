package daemon

import "time"

const (
	ServiceName         = "kwt"
	APISchemaMajor      = 1
	APISchemaVersion    = "1.2.0"
	CapabilityStatus    = "daemon.status"
	CapabilityShutdown  = "daemon.shutdown"
	CapabilityInventory = "worktree.inventory.v1"
	CapabilityRemoval   = "worktree.removal.v1"
)

type State string

const (
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateDraining State = "draining"
	StateFailed   State = "failed"
)

type Failure struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

type Status struct {
	Service       string     `json:"service"`
	State         State      `json:"state"`
	Home          string     `json:"home"`
	Endpoint      string     `json:"endpoint"`
	PID           int        `json:"pid"`
	Version       string     `json:"version"`
	Revision      string     `json:"revision"`
	SchemaMajor   int        `json:"schema_major"`
	SchemaVersion string     `json:"schema_version"`
	Capabilities  []string   `json:"capabilities"`
	StartedAt     time.Time  `json:"started_at"`
	UptimeSeconds int64      `json:"uptime_seconds"`
	ActiveWork    int        `json:"active_work"`
	ActiveLeases  int        `json:"active_leases"`
	DrainDeadline *time.Time `json:"drain_deadline,omitempty"`
	LastError     *Failure   `json:"last_error,omitempty"`
}

type ShutdownRequest struct {
	Reason string `json:"reason" enum:"stop,restart,replacement"`
}

type ShutdownResponse struct {
	Status Status `json:"status"`
}

type Problem struct {
	Type          string         `json:"type"`
	Title         string         `json:"title"`
	Status        int            `json:"status"`
	Detail        string         `json:"detail"`
	Code          string         `json:"code"`
	Retryable     bool           `json:"retryable"`
	DrainDeadline *time.Time     `json:"drain_deadline,omitempty"`
	Details       map[string]any `json:"details,omitempty"`
}
