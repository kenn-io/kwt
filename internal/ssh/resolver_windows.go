//go:build windows

package ssh

import (
	"context"

	"go.kenn.io/kit/openssh"
)

func (r *Resolver) resolveConfig(
	ctx context.Context,
	target openssh.Target,
) (openssh.EffectiveConfig, error) {
	executable, err := resolveExecutable(r.executable, r.environment, r.workingDirectory)
	if err != nil {
		return openssh.EffectiveConfig{}, err
	}
	resolver := openssh.Resolver{
		Executable: executable,
		Run: func(ctx context.Context, argv []string) ([]byte, []byte, int, error) {
			return r.run(ctx, argv, r.workingDirectory, r.environment, nil)
		},
	}
	return resolver.Resolve(ctx, target)
}
