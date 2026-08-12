package ssh

import "go.kenn.io/kwt/service"

func invalidTarget(cause error) *service.Error {
	return service.NewError(
		service.SSHInvalidTarget,
		"invalid SSH target",
		false,
		nil,
		cause,
	)
}

func resolutionFailed(cause error) *service.Error {
	return service.NewError(
		service.SSHResolutionFailed,
		"SSH configuration resolution failed",
		true,
		nil,
		cause,
	)
}

func routeUnreviewable(cause error) *service.Error {
	return service.NewError(
		service.SSHRouteUnreviewable,
		"SSH route cannot be reviewed safely",
		false,
		nil,
		cause,
	)
}

func normalizeSSHError(err error) *service.Error {
	return service.AsError(err)
}
