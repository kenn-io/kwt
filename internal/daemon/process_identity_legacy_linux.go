//go:build linux

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func readLegacyLinuxProcessIdentity(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	initTicks, err := readLegacyLinuxProcessStartTicks(1)
	if err != nil {
		return "", false
	}
	processTicks, err := readLegacyLinuxProcessStartTicks(pid)
	if err != nil {
		return "", false
	}
	identity := fmt.Sprintf(
		"%s:%s:%s",
		strings.TrimSpace(string(bootID)),
		initTicks,
		processTicks,
	)
	if !legacyLinuxProcessIdentityCompatible(identity) {
		return "", false
	}
	return identity, true
}

func legacyLinuxProcessIdentityCompatible(identity string) bool {
	parts := strings.Split(identity, ":")
	if len(parts) != 3 || !canonicalLegacyLinuxBootID(parts[0]) {
		return false
	}
	return canonicalPositiveUint(parts[1]) && canonicalPositiveUint(parts[2])
}

func canonicalLegacyLinuxBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if !('0' <= character && character <= '9') &&
				!('a' <= character && character <= 'f') {
				return false
			}
		}
	}
	return true
}

func canonicalPositiveUint(value string) bool {
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0 && strconv.FormatUint(parsed, 10) == value
}

func readLegacyLinuxProcessStartTicks(pid int) (string, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	closing := bytes.LastIndexByte(stat, ')')
	if closing < 0 || closing+1 >= len(stat) {
		return "", errors.New("Linux process stat has no command terminator")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex || !canonicalPositiveUint(fields[startTimeIndex]) {
		return "", errors.New("Linux process stat has an invalid start time")
	}
	return fields[startTimeIndex], nil
}
