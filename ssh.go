package kwt

import internalssh "go.kenn.io/kwt/internal/ssh"

type (
	SSHTarget              = internalssh.Target
	SSHResolveRequest      = internalssh.ResolveRequest
	SSHExecutionProjection = internalssh.ExecutionProjection
	SSHResolvedTarget      = internalssh.ResolvedTarget
	SSHRouteSnapshot       = internalssh.RouteSnapshot
	SSHService             = internalssh.Service
)

const SSHProjectionPolicyV1 = internalssh.ProjectionPolicyV1

func NewSSHService() *SSHService {
	return internalssh.NewService(internalssh.ServiceOptions{})
}
