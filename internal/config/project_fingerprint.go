package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const (
	projectRegistrationFingerprintPrefix = "v1:"
	projectRegistrationFingerprintDomain = "kwt.project-registration"
)

var (
	timeType          = reflect.TypeFor[time.Time]()
	localDateType     = reflect.TypeFor[toml.LocalDate]()
	localTimeType     = reflect.TypeFor[toml.LocalTime]()
	localDateTimeType = reflect.TypeFor[toml.LocalDateTime]()
)

// Fingerprint returns an opaque concurrency token for the exact decoded
// project registration. The final raw-entry compare-and-swap remains the
// mutation authority; this token lets remote clients bind their request to the
// same observed entry before the guarded removal transaction begins.
func (p ProjectRegistration) Fingerprint() (string, error) {
	if p.raw == nil {
		return "", errors.New("project registration has no persisted entry")
	}

	encoder := projectFingerprintEncoder{}
	encoder.writeString(projectRegistrationFingerprintDomain)
	encoder.writeString(projectRegistrationFingerprintPrefix)
	if err := encoder.writeValue(reflect.ValueOf(p.raw)); err != nil {
		return "", fmt.Errorf("fingerprint project registration: %w", err)
	}
	sum := sha256.Sum256(encoder.Bytes())
	return projectRegistrationFingerprintPrefix + hex.EncodeToString(sum[:]), nil
}

// ValidProjectRegistrationFingerprint reports whether value is a canonical
// project-registration fingerprint token for the current encoding version.
func ValidProjectRegistrationFingerprint(value string) bool {
	if len(value) != len(projectRegistrationFingerprintPrefix)+sha256.Size*2 ||
		value[:len(projectRegistrationFingerprintPrefix)] != projectRegistrationFingerprintPrefix {
		return false
	}
	for _, character := range value[len(projectRegistrationFingerprintPrefix):] {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// sameProjectRegistrationRaw compares decoded project entries using the same
// floating-point identity that the fingerprint encoder publishes. TOML NaN
// values are unchanged when their IEEE representation is unchanged, even
// though reflect.DeepEqual considers every NaN unequal to itself.
func sameProjectRegistrationRaw(left, right map[string]any) bool {
	return sameProjectRegistrationValue(
		reflect.ValueOf(left),
		reflect.ValueOf(right),
	)
}

func sameProjectRegistrationValue(left, right reflect.Value) bool {
	if !left.IsValid() || !right.IsValid() {
		return left.IsValid() == right.IsValid()
	}
	if left.Type() != right.Type() {
		return false
	}

	switch left.Kind() {
	case reflect.Interface:
		if left.IsNil() || right.IsNil() {
			return left.IsNil() == right.IsNil()
		}
		return sameProjectRegistrationValue(left.Elem(), right.Elem())
	case reflect.Float32:
		return math.Float32bits(float32(left.Float())) ==
			math.Float32bits(float32(right.Float()))
	case reflect.Float64:
		return math.Float64bits(left.Float()) == math.Float64bits(right.Float())
	case reflect.Array:
		for index := range left.Len() {
			if !sameProjectRegistrationValue(left.Index(index), right.Index(index)) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if left.IsNil() != right.IsNil() || left.Len() != right.Len() {
			return false
		}
		for index := range left.Len() {
			if !sameProjectRegistrationValue(left.Index(index), right.Index(index)) {
				return false
			}
		}
		return true
	case reflect.Map:
		if left.IsNil() != right.IsNil() || left.Len() != right.Len() {
			return false
		}
		for _, key := range left.MapKeys() {
			leftValue := left.MapIndex(key)
			rightValue := right.MapIndex(key)
			if !rightValue.IsValid() ||
				!sameProjectRegistrationValue(leftValue, rightValue) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(left.Interface(), right.Interface())
	}
}

type projectFingerprintEncoder struct {
	bytes.Buffer
}

func (e *projectFingerprintEncoder) writeValue(value reflect.Value) error {
	for value.IsValid() && value.Kind() == reflect.Interface {
		if value.IsNil() {
			return errors.New("nil interface value is unsupported")
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return errors.New("invalid value is unsupported")
	}

	typeOfValue := value.Type()
	e.writeType(typeOfValue)
	if value.CanInterface() {
		switch typeOfValue {
		case timeType:
			e.writeTime(value.Interface().(time.Time))
			return nil
		case localDateType:
			e.writeLocalDate(value.Interface().(toml.LocalDate))
			return nil
		case localTimeType:
			e.writeLocalTime(value.Interface().(toml.LocalTime))
			return nil
		case localDateTimeType:
			local := value.Interface().(toml.LocalDateTime)
			e.writeLocalDate(local.LocalDate)
			e.writeLocalTime(local.LocalTime)
			return nil
		}
	}

	switch value.Kind() {
	case reflect.Bool:
		e.writeBool(value.Bool())
	case reflect.String:
		e.writeString(value.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.writeUint64(uint64(value.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		e.writeUint64(value.Uint())
	case reflect.Float32:
		e.writeUint64(uint64(math.Float32bits(float32(value.Float()))))
	case reflect.Float64:
		e.writeUint64(math.Float64bits(value.Float()))
	case reflect.Array:
		e.writeUint64(uint64(value.Len()))
		for index := range value.Len() {
			if err := e.writeValue(value.Index(index)); err != nil {
				return err
			}
		}
	case reflect.Slice:
		e.writeBool(value.IsNil())
		e.writeUint64(uint64(value.Len()))
		for index := range value.Len() {
			if err := e.writeValue(value.Index(index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("map key type %s is unsupported", value.Type().Key())
		}
		e.writeBool(value.IsNil())
		keys := value.MapKeys()
		sort.Slice(keys, func(left, right int) bool {
			return keys[left].String() < keys[right].String()
		})
		e.writeUint64(uint64(len(keys)))
		for _, key := range keys {
			e.writeString(key.String())
			if err := e.writeValue(value.MapIndex(key)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("value type %s is unsupported", typeOfValue)
	}
	return nil
}

func (e *projectFingerprintEncoder) writeType(value reflect.Type) {
	e.writeString(value.PkgPath())
	e.writeString(value.String())
	e.WriteByte(byte(value.Kind()))
}

func (e *projectFingerprintEncoder) writeTime(value time.Time) {
	e.writeUint64(uint64(value.Year()))
	e.writeUint64(uint64(value.Month()))
	e.writeUint64(uint64(value.Day()))
	e.writeUint64(uint64(value.Hour()))
	e.writeUint64(uint64(value.Minute()))
	e.writeUint64(uint64(value.Second()))
	e.writeUint64(uint64(value.Nanosecond()))
	zoneName, zoneOffset := value.Zone()
	e.writeString(zoneName)
	e.writeUint64(uint64(zoneOffset))
}

func (e *projectFingerprintEncoder) writeLocalDate(value toml.LocalDate) {
	e.writeUint64(uint64(value.Year))
	e.writeUint64(uint64(value.Month))
	e.writeUint64(uint64(value.Day))
}

func (e *projectFingerprintEncoder) writeLocalTime(value toml.LocalTime) {
	e.writeUint64(uint64(value.Hour))
	e.writeUint64(uint64(value.Minute))
	e.writeUint64(uint64(value.Second))
	e.writeUint64(uint64(value.Nanosecond))
	e.writeUint64(uint64(value.Precision))
}

func (e *projectFingerprintEncoder) writeBool(value bool) {
	if value {
		e.WriteByte(1)
		return
	}
	e.WriteByte(0)
}

func (e *projectFingerprintEncoder) writeString(value string) {
	e.writeBytes([]byte(value))
}

func (e *projectFingerprintEncoder) writeBytes(value []byte) {
	var length [binary.MaxVarintLen64]byte
	written := binary.PutUvarint(length[:], uint64(len(value)))
	e.Write(length[:written])
	e.Write(value)
}

func (e *projectFingerprintEncoder) writeUint64(value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	e.Write(encoded[:])
}
