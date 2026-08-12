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

func readStableProcessIdentity(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(bootID)) == "" {
		return "", false
	}
	initTicks, err := readLinuxProcessStartTicks(1)
	if err != nil {
		return "", false
	}
	processTicks, err := readLinuxProcessStartTicks(pid)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf(
		"%s:%s:%s",
		strings.TrimSpace(string(bootID)),
		initTicks,
		processTicks,
	), true
}

func readLinuxProcessStartTicks(pid int) (string, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	return parseLinuxProcessStartTicks(stat)
}

func parseLinuxProcessStartTicks(stat []byte) (string, error) {
	closing := bytes.LastIndexByte(stat, ')')
	if closing < 0 || closing+1 >= len(stat) {
		return "", errors.New("Linux process stat has no command terminator")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	const startTimeIndex = 19 // Field 22 after fields 1 (PID) and 2 (command).
	if len(fields) <= startTimeIndex {
		return "", errors.New("Linux process stat has no start time")
	}
	if _, err := strconv.ParseUint(fields[startTimeIndex], 10, 64); err != nil {
		return "", errors.New("Linux process stat has an invalid start time")
	}
	return fields[startTimeIndex], nil
}
