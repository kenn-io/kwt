package kwt

import (
	"io"

	internalssh "go.kenn.io/kwt/internal/ssh"
)

type (
	SSHTarget              = internalssh.Target
	SSHResolveRequest      = internalssh.ResolveRequest
	SSHExecutionProjection = internalssh.ExecutionProjection
	SSHResolvedTarget      = internalssh.ResolvedTarget
	SSHRouteSnapshot       = internalssh.RouteSnapshot
	SSHLeaseRequest        = internalssh.LeaseRequest
	SSHHostKeyPolicy       = internalssh.HostKeyPolicy
	SSHPromptHandler       = internalssh.PromptHandler
	SSHLease               = internalssh.Lease
	SSHLeaseMode           = internalssh.LeaseMode
	SSHEvent               = internalssh.Event
	SSHServiceOptions      = internalssh.PublicServiceOptions
	SSHService             = internalssh.PublicService
)

const (
	SSHHostKeyPolicyReview = internalssh.HostKeyPolicyReview
	SSHHostKeyPolicyStrict = internalssh.HostKeyPolicyStrict
)

const (
	SSHProjectionPolicyV1     = internalssh.ProjectionPolicyV1
	SSHLeaseModeMultiplexed   = internalssh.LeaseModeMultiplexed
	SSHLeaseModeMasterless    = internalssh.LeaseModeMasterless
	SSHEventStateConnected    = internalssh.EventStateConnected
	SSHEventStateDisconnected = internalssh.EventStateDisconnected
	SSHEventStateError        = internalssh.EventStateError
)

func NewSSHService(options SSHServiceOptions) *SSHService {
	return internalssh.NewPublicService(options)
}

// RunSSHAskpassHelper handles an SSH askpass invocation before host startup.
func RunSSHAskpassHelper(
	arguments, environment []string,
	output io.Writer,
) (int, bool) {
	return internalssh.RunAskpassHelper(arguments, environment, output)
}
