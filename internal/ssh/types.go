package ssh

import (
	"time"

	"go.kenn.io/kit/openssh"
)

type Target struct {
	Hostname string `json:"hostname"`
	User     string `json:"user,omitempty"`
	Port     int    `json:"port,omitempty"`
}

func (t Target) Display() string {
	return t.openSSH().String()
}

func (t Target) CommandDestination() (string, int) {
	return t.openSSH().CommandDestination()
}

func (t Target) openSSH() openssh.Target {
	return openssh.Target{User: t.User, Hostname: t.Hostname, Port: t.Port}
}

func targetFromOpenSSH(target openssh.Target) Target {
	return Target{User: target.User, Hostname: target.Hostname, Port: target.Port}
}

type ResolveRequest struct {
	Target           Target   `json:"target"`
	WorkingDirectory string   `json:"working_directory"`
	Environment      []string `json:"environment"`
}

type ExecutionProjection struct {
	Arguments     []string `json:"arguments"`
	PrivateConfig []string `json:"private_config,omitempty"`
}

type ResolvedTarget struct {
	LogicalTarget         Target              `json:"logical_target"`
	EffectiveTarget       Target              `json:"effective_target"`
	DisplayTarget         string              `json:"display_target"`
	HostKeyAlias          string              `json:"host_key_alias,omitempty"`
	StrictHostKeyChecking string              `json:"strict_host_key_checking,omitempty"`
	Projection            ExecutionProjection `json:"projection"`
}

type RouteSnapshot struct {
	LogicalTarget Target `json:"logical_target"`
	// Targets are ordered in connection order. A downstream projection is
	// target-local and requires proxy transport through the preceding target.
	Targets          []ResolvedTarget `json:"targets"`
	RouteIdentity    string           `json:"route_identity"`
	ProjectionPolicy string           `json:"projection_policy"`
	ObservedAt       time.Time        `json:"observed_at"`
}

// routeObservation is the private resolver result. The complete normalized
// OpenSSH stream remains reachable only through this internal route and is
// never embedded in RouteSnapshot.
type routeObservation struct {
	route openssh.Route
}
