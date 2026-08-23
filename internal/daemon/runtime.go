package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kwt/service"
	"golang.org/x/mod/semver"
)

const (
	RuntimePrefix                      = "kwt"
	metadataHome                       = "home"
	metadataRevision                   = "revision"
	metadataRevisionTime               = "revision_time"
	metadataSchemaMajor                = "schema_major"
	metadataSchemaVersion              = "schema_version"
	metadataCapabilities               = "capabilities"
	metadataToken                      = "bearer"
	metadataLegacyLinuxProcessIdentity = "process_identity_linux_v1"
)

type Build struct {
	Version      string
	Revision     string
	RevisionTime string
}

type RuntimeState uint8

const (
	RuntimeAbsent RuntimeState = iota
	RuntimeReady
	RuntimeStarting
	RuntimeFailed
	RuntimeDraining
	RuntimeIncompatible
	RuntimeUnresponsive
)

type Observation struct {
	State  RuntimeState
	Record kitdaemon.RuntimeRecord
	Status Status
	Token  string
	Client *Client
	Err    error
}

type runtimeMetadata struct {
	home                   string
	revision               string
	revisionTime           string
	revisionTimeAdvertised bool
	schemaMajor            int
	schemaVersion          string
	capabilities           []string
	token                  string
}

func RuntimeStore(home string) kitdaemon.RuntimeStore {
	return kitdaemon.RuntimeStore{
		Dir:    filepath.Join(home, "runtime"),
		Prefix: RuntimePrefix,
	}
}

func NewRuntimeRecord(
	home string,
	build Build,
	ep kitdaemon.Endpoint,
) (kitdaemon.RuntimeRecord, string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return kitdaemon.RuntimeRecord{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	rec := kitdaemon.NewRuntimeRecord(ServiceName, build.Version, ep)
	rec.Metadata = map[string]string{
		metadataHome:          home,
		metadataRevision:      build.Revision,
		metadataRevisionTime:  build.RevisionTime,
		metadataSchemaMajor:   strconv.Itoa(APISchemaMajor),
		metadataSchemaVersion: APISchemaVersion,
		metadataCapabilities: strings.Join([]string{
			CapabilityShutdown,
			CapabilityStatus,
			CapabilityOperationStream,
			CapabilityProjectRemoval,
			CapabilitySSHLeaseHold,
			CapabilitySSHLifecycle,
			CapabilitySSHResolve,
			CapabilityInventoryConfig,
			CapabilityInventory,
			CapabilityRemoval,
			CapabilityGuardedRemoval,
		}, ","),
		metadataToken: token,
	}
	return rec, token, nil
}

func Inspect(
	ctx context.Context,
	store kitdaemon.RuntimeStore,
	home string,
) (Observation, error) {
	records, err := store.List()
	if err != nil {
		return Observation{}, err
	}
	var found *Observation
	for _, rec := range records {
		if rec.Service != ServiceName || rec.Metadata[metadataHome] != home {
			continue
		}
		if !kitdaemon.ProcessAlive(rec.PID) {
			if err := removeStaleRecord(store, rec); err != nil {
				return Observation{}, err
			}
			continue
		}
		var candidate Observation
		switch compareRuntimeProcessIdentity(rec) {
		case kitdaemon.ProcessIdentityMismatch:
			if err := removeStaleRecord(store, rec); err != nil {
				return Observation{}, err
			}
			continue
		case kitdaemon.ProcessIdentityMatch:
			candidate = inspectLiveRecord(ctx, rec, home)
		default:
			candidate = Observation{
				State:  RuntimeUnresponsive,
				Record: rec,
				Err:    errors.New("daemon process identity cannot be verified"),
			}
		}
		if found != nil {
			return Observation{}, service.NewError(
				service.Conflict,
				"multiple kwt daemon owners found",
				false,
				nil,
				nil,
			)
		}
		found = &candidate
	}
	if found == nil {
		return Observation{State: RuntimeAbsent}, nil
	}
	return *found, nil
}

func compareRuntimeProcessIdentity(
	rec kitdaemon.RuntimeRecord,
) kitdaemon.ProcessIdentityStatus {
	if rec.ProcessIdentityV2 != "" {
		return kitdaemon.CompareRuntimeProcessIdentity(rec)
	}
	recorded, legacy := rec.Metadata[metadataLegacyLinuxProcessIdentity]
	if !legacy {
		return kitdaemon.CompareRuntimeProcessIdentity(rec)
	}
	live, ok := readLegacyLinuxProcessIdentity(rec.PID)
	if recorded == "" || !ok || !legacyLinuxProcessIdentityCompatible(recorded) {
		return kitdaemon.ProcessIdentityUnknown
	}
	if live == recorded {
		return kitdaemon.ProcessIdentityMatch
	}
	return kitdaemon.ProcessIdentityMismatch
}

func inspectLiveRecord(
	ctx context.Context,
	rec kitdaemon.RuntimeRecord,
	home string,
) Observation {
	observation := Observation{
		State:  RuntimeUnresponsive,
		Record: rec,
	}
	metadata, err := parseRuntimeMetadata(rec, home)
	if err != nil {
		observation.Err = err
		return observation
	}
	observation.Token = metadata.token
	client, err := NewVerifiedClient(ctx, rec, metadata.token)
	if err != nil {
		observation.Err = err
		return observation
	}
	status, err := client.Status(ctx)
	if err != nil {
		observation.Err = err
		return observation
	}
	if err := validateRuntimeStatus(rec, metadata, status); err != nil {
		observation.Err = err
		return observation
	}
	client.capabilities = slices.Clone(status.Capabilities)
	observation.Client = client
	observation.Status = status
	observation.State, observation.Err = classifyRuntimeStatus(status)
	return observation
}

func classifyRuntimeStatus(status Status) (RuntimeState, error) {
	if status.SchemaMajor != APISchemaMajor {
		return RuntimeIncompatible, nil
	}
	switch status.State {
	case StateReady:
		return RuntimeReady, nil
	case StateStarting:
		return RuntimeStarting, nil
	case StateFailed:
		return RuntimeFailed, nil
	case StateDraining:
		return RuntimeDraining, nil
	default:
		return RuntimeUnresponsive, fmt.Errorf(
			"daemon reported unknown state %q",
			status.State,
		)
	}
}

func parseRuntimeMetadata(
	rec kitdaemon.RuntimeRecord,
	home string,
) (runtimeMetadata, error) {
	if rec.Service != ServiceName {
		return runtimeMetadata{}, fmt.Errorf("runtime service is not %s", ServiceName)
	}
	metadata := runtimeMetadata{
		home:          rec.Metadata[metadataHome],
		revision:      rec.Metadata[metadataRevision],
		schemaVersion: rec.Metadata[metadataSchemaVersion],
		token:         rec.Metadata[metadataToken],
	}
	metadata.revisionTime, metadata.revisionTimeAdvertised = rec.Metadata[metadataRevisionTime]
	if metadata.home == "" || metadata.home != home {
		return runtimeMetadata{}, errors.New("runtime home does not match")
	}
	if metadata.token == "" {
		return runtimeMetadata{}, errors.New("runtime bearer token is missing")
	}
	major, err := strconv.Atoi(rec.Metadata[metadataSchemaMajor])
	if err != nil || major <= 0 {
		return runtimeMetadata{}, errors.New("runtime schema major is invalid")
	}
	metadata.schemaMajor = major
	if metadata.schemaVersion == "" {
		return runtimeMetadata{}, errors.New("runtime schema version is missing")
	}
	schemaVersion, schemaVersionValid := comparableVersion(metadata.schemaVersion)
	if !schemaVersionValid {
		return runtimeMetadata{}, errors.New("runtime schema version is invalid")
	}
	if semver.Compare(schemaVersion, "v1.3.0") >= 0 &&
		!metadata.revisionTimeAdvertised {
		return runtimeMetadata{}, errors.New("runtime revision time is missing")
	}
	if metadata.revisionTime != "" {
		if _, valid := parseRevisionTime(metadata.revisionTime); !valid {
			return runtimeMetadata{}, errors.New("runtime revision time is invalid")
		}
	}
	metadata.capabilities, err = parseCapabilities(rec.Metadata[metadataCapabilities])
	if err != nil {
		return runtimeMetadata{}, err
	}
	return metadata, nil
}

func parseCapabilities(raw string) ([]string, error) {
	if raw == "" {
		return nil, errors.New("runtime capabilities are missing")
	}
	capabilities := strings.Split(raw, ",")
	for index, capability := range capabilities {
		if capability == "" || strings.TrimSpace(capability) != capability {
			return nil, errors.New("runtime capabilities are invalid")
		}
		if index > 0 && capabilities[index-1] >= capability {
			return nil, errors.New("runtime capabilities must be sorted and unique")
		}
	}
	return capabilities, nil
}

func validateRuntimeStatus(
	rec kitdaemon.RuntimeRecord,
	metadata runtimeMetadata,
	status Status,
) error {
	if status.Service != ServiceName || status.Home != metadata.home ||
		status.PID != rec.PID || status.Endpoint != rec.Endpoint().Address {
		return errors.New("daemon status does not match its runtime record")
	}
	if status.Version != rec.Version || status.Revision != metadata.revision ||
		status.RevisionTime != metadata.revisionTime {
		return errors.New("daemon build identity does not match its runtime record")
	}
	if status.SchemaMajor <= 0 || status.SchemaMajor != metadata.schemaMajor ||
		status.SchemaVersion == "" || status.SchemaVersion != metadata.schemaVersion {
		return errors.New("daemon schema does not match its runtime record")
	}
	capabilities, err := parseCapabilities(strings.Join(status.Capabilities, ","))
	if err != nil {
		return err
	}
	if !slices.Equal(capabilities, metadata.capabilities) {
		return errors.New("daemon capabilities do not match its runtime record")
	}
	return nil
}

func removeStaleRecord(
	store kitdaemon.RuntimeStore,
	rec kitdaemon.RuntimeRecord,
) error {
	expected, err := store.Path(rec.PID)
	if err != nil {
		return err
	}
	if rec.SourcePath != expected {
		return fmt.Errorf("runtime record path %q does not match %q", rec.SourcePath, expected)
	}
	if err := os.Remove(expected); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale runtime record: %w", err)
	}
	return nil
}
