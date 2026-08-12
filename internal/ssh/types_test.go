package ssh

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
)

func TestTargetUsesCredentialFreeOpenSSHDisplayGrammar(t *testing.T) {
	target := Target{User: "deploy", Hostname: "2001:db8::42", Port: 2200}
	assert.Equal(t, "deploy@[2001:db8::42]:2200", target.Display())
	destination, port := target.CommandDestination()
	assert.Equal(t, "deploy@2001:db8::42", destination)
	assert.Equal(t, 2200, port)
}

func TestRouteSnapshotJSONOmitsCanonicalConfiguration(t *testing.T) {
	snapshot := RouteSnapshot{
		LogicalTarget:    Target{User: "deploy", Hostname: "build.example.test"},
		RouteIdentity:    strings.Repeat("a", 64),
		ProjectionPolicy: projectionPolicyV1,
		ObservedAt:       time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		Targets: []ResolvedTarget{{
			LogicalTarget: Target{Hostname: "build.example.test"},
			EffectiveTarget: Target{
				User: "deploy", Hostname: "build.internal", Port: 22,
			},
			DisplayTarget:         "deploy@build.internal:22",
			StrictHostKeyChecking: "ask",
			Projection: ExecutionProjection{
				Arguments: []string{"-F", "/dev/null", "-o", "User=deploy"},
			},
		}},
	}

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "canonical_options")
	assert.NotContains(t, string(encoded), "localforward")
	assert.Contains(t, string(encoded), `"route_identity":"`+strings.Repeat("a", 64)+`"`)
}

func TestSSHServiceErrorsPreserveStableCodeAndPrivateCause(t *testing.T) {
	cause := errors.New("private resolver diagnostic")
	err := resolutionFailed(cause)
	assert.Equal(t, service.SSHResolutionFailed, err.Code)
	assert.Equal(t, "SSH configuration resolution failed", err.Message)
	assert.ErrorIs(t, err, cause)

	wrapped := service.NewError(
		service.SSHRouteUnreviewable,
		"SSH route cannot be reviewed safely",
		false,
		nil,
		cause,
	)
	assert.Same(t, wrapped, normalizeSSHError(wrapped))

	internal := normalizeSSHError(cause)
	assert.Equal(t, service.Internal, internal.Code)
	assert.Equal(t, "internal failure", internal.Message)
	assert.ErrorIs(t, internal, cause)
}
