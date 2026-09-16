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

// SSHProjectionPolicyV1 identifies the original SSH execution policy.
//
// Deprecated: Newly resolved routes use SSHProjectionPolicyV2.
const SSHProjectionPolicyV1 = "kwt.openssh.projection.v1"

const (
	SSHProjectionPolicyV2     = internalssh.ProjectionPolicyV2
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
