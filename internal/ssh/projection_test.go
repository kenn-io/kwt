package ssh

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
)

type projectionFixture struct {
	Name   string `json:"name"`
	Config struct {
		User         string           `json:"user"`
		Hostname     string           `json:"hostname"`
		Port         int              `json:"port"`
		HostKeyAlias string           `json:"host_key_alias"`
		Options      []openssh.Option `json:"options"`
	} `json:"config"`
	Arguments     []string `json:"arguments"`
	PrivateConfig []string `json:"private_config"`
	Excluded      []string `json:"excluded"`
}

func TestProjectionV1MatchesGhosthubSelectiveReplay(t *testing.T) {
	data, err := os.ReadFile("testdata/projection_v1.json")
	require.NoError(t, err)
	var fixtures []projectionFixture
	require.NoError(t, json.Unmarshal(data, &fixtures))

	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			projected, err := projectConfig(openssh.EffectiveConfig{
				User:         fixture.Config.User,
				Hostname:     fixture.Config.Hostname,
				Port:         fixture.Config.Port,
				HostKeyAlias: fixture.Config.HostKeyAlias,
				Options:      fixture.Config.Options,
			})
			require.NoError(t, err)
			assert.Equal(t, projectionPolicyV1, projected.PolicyVersion)
			assert.Equal(t, fixture.Arguments, projected.Arguments)
			assert.Equal(t, fixture.PrivateConfig, projected.PrivateConfig)

			execution := strings.Join(
				append(append([]string{}, projected.Arguments...), projected.PrivateConfig...),
				"\n",
			)
			for _, excluded := range fixture.Excluded {
				assert.NotContains(t, strings.ToLower(execution), strings.ToLower(excluded))
			}
		})
	}
}

func TestProjectionV1SafelyQuotesPrivateValues(t *testing.T) {
	projected, err := projectConfig(openssh.EffectiveConfig{Options: []openssh.Option{
		{Name: "identityfile", Value: `/credentials/id "quoted"\key`},
		{Name: "setenv", Value: `CHANNEL=value with spaces`},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		`IdentityFile "/credentials/id \"quoted\"\\key"`,
		`SetEnv "CHANNEL=value with spaces"`,
	}, projected.PrivateConfig)
}

func TestProjectionV1RejectsMultilinePrivateValues(t *testing.T) {
	_, err := projectConfig(openssh.EffectiveConfig{Options: []openssh.Option{
		{Name: "setenv", Value: "SAFE=value\nInclude /tmp/hostile"},
	}})
	require.Error(t, err)
}

func TestProjectionV1IdentityIncludesFullConfigAndPolicy(t *testing.T) {
	route := openssh.Route{{
		Target: openssh.Target{User: "deploy", Hostname: "build.example.test", Port: 22},
		Config: openssh.EffectiveConfig{
			User: "deploy", Hostname: "build.internal", Port: 22,
			Options: []openssh.Option{{Name: "localforward", Value: "8080 localhost:80"}},
		},
	}}

	identity := routeIdentity(projectionPolicyV1, route)
	changedConfig := append(openssh.Route(nil), route...)
	changedConfig[0].Config.Options = []openssh.Option{{
		Name: "localforward", Value: "8081 localhost:80",
	}}
	assert.NotEqual(t, identity, routeIdentity(projectionPolicyV1, changedConfig))
	assert.NotEqual(t, identity, routeIdentity("kwt.openssh.projection.v2", route))
}
