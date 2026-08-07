package git

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequireVersionOutput(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		major      int
		minor      int
		wantErr    bool
		errorParts []string
	}{
		{
			name:   "general minimum",
			output: "git version 2.20.0\n",
			major:  2,
			minor:  20,
		},
		{
			name:       "maintenance version too old",
			output:     "git version 2.30.2\n",
			major:      2,
			minor:      31,
			wantErr:    true,
			errorParts: []string{"2.31", "2.30.2"},
		},
		{
			name:   "maintenance minimum",
			output: "git version 2.31.0\n",
			major:  2,
			minor:  31,
		},
		{
			name:   "newer major",
			output: "git version 3.0.0\n",
			major:  2,
			minor:  31,
		},
		{
			name:   "windows suffix",
			output: "git version 2.55.0.windows.3\n",
			major:  2,
			minor:  31,
		},
		{
			name:       "malformed output",
			output:     "git unknown\n",
			major:      2,
			minor:      31,
			wantErr:    true,
			errorParts: []string{"unexpected Git version output", "git unknown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireVersionOutput(tt.output, tt.major, tt.minor)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, part := range tt.errorParts {
				require.ErrorContains(t, err, part)
			}
		})
	}
}
