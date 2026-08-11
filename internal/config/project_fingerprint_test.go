package config

import (
	"math"
	"math/rand"
	"reflect"
	"regexp"
	"testing"
	"testing/quick"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"
)

func TestProjectRegistrationFingerprintIsDeterministic(t *testing.T) {
	t.Parallel()

	left := ProjectRegistration{raw: map[string]any{
		"path":       "/repo",
		"repository": "github.com/kenn-io/kwt",
		"metadata": map[string]any{
			"count": int64(3),
			"tags":  []any{"one", "two"},
		},
	}}
	right := ProjectRegistration{raw: map[string]any{
		"metadata": map[string]any{
			"tags":  []any{"one", "two"},
			"count": int64(3),
		},
		"repository": "github.com/kenn-io/kwt",
		"path":       "/repo",
	}}

	leftFingerprint, err := left.Fingerprint()
	require.NoError(t, err)
	rightFingerprint, err := right.Fingerprint()
	require.NoError(t, err)
	require.Equal(t, leftFingerprint, rightFingerprint)
	require.Regexp(t, regexp.MustCompile(`^v1:[0-9a-f]{64}$`), leftFingerprint)
	require.True(t, ValidProjectRegistrationFingerprint(leftFingerprint))
}

func TestProjectRegistrationFingerprintPreservesDistinctValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		left  any
		right any
	}{
		{name: "integer width", left: int32(5), right: int64(5)},
		{name: "integer signedness", left: int64(5), right: uint64(5)},
		{name: "integer and string", left: int64(5), right: "5"},
		{name: "array order", left: []any{"one", "two"}, right: []any{"two", "one"}},
		{name: "nil and empty slice", left: []any(nil), right: []any{}},
		{name: "nil and empty map", left: map[string]any(nil), right: map[string]any{}},
		{name: "unknown field", left: map[string]any{"path": "/repo"}, right: map[string]any{"path": "/repo", "future": true}},
		{name: "local time precision", left: toml.LocalTime{Hour: 1, Precision: 3}, right: toml.LocalTime{Hour: 1, Precision: 6}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			left := fingerprintValue(t, test.left)
			right := fingerprintValue(t, test.right)
			require.NotEqual(t, left, right)
		})
	}
}

func TestProjectRegistrationFingerprintPreservesDatetimeRepresentation(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, time.August, 11, 12, 34, 56, 789, time.FixedZone("CDT", -5*60*60))
	sameInstant := instant.In(time.FixedZone("EDT", -4*60*60))
	require.True(t, instant.Equal(sameInstant))
	require.False(t, reflect.DeepEqual(instant, sameInstant))
	require.NotEqual(t, fingerprintValue(t, instant), fingerprintValue(t, sameInstant))
}

func TestProjectRegistrationFingerprintPreservesNaNBits(t *testing.T) {
	t.Parallel()

	positive := math.Float64frombits(0x7ff8000000000001)
	differentPayload := math.Float64frombits(0x7ff8000000000002)
	negative := math.Float64frombits(0xfff8000000000001)

	require.NotEqual(t, fingerprintValue(t, positive), fingerprintValue(t, differentPayload))
	require.NotEqual(t, fingerprintValue(t, positive), fingerprintValue(t, negative))
}

func TestProjectRegistrationFingerprintRejectsUnsupportedGraphs(t *testing.T) {
	t.Parallel()

	value := "secret"
	tests := []struct {
		name string
		raw  map[string]any
	}{
		{name: "missing raw registration", raw: nil},
		{name: "pointer", raw: map[string]any{"value": &value}},
		{name: "non-string map key", raw: map[string]any{"value": map[int]string{1: "one"}}},
		{name: "arbitrary struct", raw: map[string]any{"value": struct{ Value string }{Value: "one"}}},
		{name: "function", raw: map[string]any{"value": func() {}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := (ProjectRegistration{raw: test.raw}).Fingerprint()
			require.Error(t, err)
		})
	}
}

func TestProjectRegistrationFingerprintValidation(t *testing.T) {
	t.Parallel()

	valid := "v1:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.True(t, ValidProjectRegistrationFingerprint(valid))

	for _, invalid := range []string{
		"",
		"v2:" + valid[3:],
		"v1:0123",
		"v1:" + "0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",
		"v1:" + "g123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		require.False(t, ValidProjectRegistrationFingerprint(invalid), invalid)
	}
}

func TestProjectRegistrationFingerprintGeneratedGraphs(t *testing.T) {
	t.Parallel()

	property := func(seed uint64) bool {
		random := rand.New(rand.NewSource(int64(seed)))
		left := generatedFingerprintGraph(random, 0)
		right := cloneFingerprintGraph(left)

		leftFingerprint, leftErr := (ProjectRegistration{raw: map[string]any{"value": left}}).Fingerprint()
		rightFingerprint, rightErr := (ProjectRegistration{raw: map[string]any{"value": right}}).Fingerprint()
		return leftErr == nil && rightErr == nil && reflect.DeepEqual(left, right) && leftFingerprint == rightFingerprint
	}

	err := quick.Check(property, &quick.Config{
		MaxCount: 250,
		Rand:     rand.New(rand.NewSource(0x6b7774)),
	})
	require.NoError(t, err)
}

func fingerprintValue(t *testing.T, value any) string {
	t.Helper()
	fingerprint, err := (ProjectRegistration{raw: map[string]any{"value": value}}).Fingerprint()
	require.NoError(t, err)
	return fingerprint
}

func generatedFingerprintGraph(random *rand.Rand, depth int) any {
	if depth >= 3 {
		return generatedFingerprintLeaf(random)
	}
	switch random.Intn(3) {
	case 0:
		return generatedFingerprintLeaf(random)
	case 1:
		values := make([]any, random.Intn(5))
		for index := range values {
			values[index] = generatedFingerprintGraph(random, depth+1)
		}
		return values
	default:
		values := make(map[string]any)
		for index := 0; index < random.Intn(5); index++ {
			values[string(rune('a'+index))] = generatedFingerprintGraph(random, depth+1)
		}
		return values
	}
}

func generatedFingerprintLeaf(random *rand.Rand) any {
	switch random.Intn(7) {
	case 0:
		return random.Intn(2) == 1
	case 1:
		return int64(random.Int63())
	case 2:
		return uint32(random.Uint32())
	case 3:
		return math.Float64frombits(random.Uint64())
	case 4:
		return string([]byte{byte(random.Intn(256)), byte(random.Intn(256))})
	case 5:
		return toml.LocalDate{Year: 2000 + random.Intn(50), Month: 1 + random.Intn(12), Day: 1 + random.Intn(28)}
	default:
		return time.Date(2000+random.Intn(50), 1, 1, 0, 0, 0, random.Intn(1_000_000_000), time.FixedZone("generated", random.Intn(24*60*60)-12*60*60))
	}
}

func cloneFingerprintGraph(value any) any {
	switch typed := value.(type) {
	case []any:
		cloned := make([]any, len(typed))
		for index := range typed {
			cloned[index] = cloneFingerprintGraph(typed[index])
		}
		return cloned
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, element := range typed {
			cloned[key] = cloneFingerprintGraph(element)
		}
		return cloned
	default:
		return value
	}
}
