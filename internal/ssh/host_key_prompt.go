package ssh

import (
	"errors"
	"strconv"
	"strings"
)

type hostKeyPromptDetails struct {
	Host        string
	Algorithm   string
	Fingerprint string
}

func parseHostKeyPrompt(message string) (hostKeyPromptDetails, error) {
	const (
		hostPrefix = "The authenticity of host '"
		hostSuffix = "' can't be established."
		question   = "Are you sure you want to continue connecting (yes/no/[fingerprint])?"
	)
	fingerprintMarkers := []string{
		" key fingerprint is: ",
		" key fingerprint is ",
	}

	message = strings.ReplaceAll(message, "\r\n", "\n")
	if strings.ContainsRune(message, '\r') {
		return hostKeyPromptDetails{}, errors.New("OpenSSH host-key prompt is missing review details")
	}
	lines := strings.Split(message, "\n")
	if len(lines) < 3 ||
		!strings.HasPrefix(lines[0], hostPrefix) ||
		!strings.HasSuffix(lines[0], hostSuffix) ||
		strings.TrimRight(lines[len(lines)-1], " \t") != question {
		return hostKeyPromptDetails{}, errors.New("OpenSSH host-key prompt is missing review details")
	}
	reviewLines := lines[2 : len(lines)-1]
	if !validHostKeyReviewLines(reviewLines) {
		return hostKeyPromptDetails{}, errors.New("OpenSSH host-key prompt is missing review details")
	}

	details := hostKeyPromptDetails{
		Host: strings.TrimSuffix(
			strings.TrimPrefix(lines[0], hostPrefix),
			hostSuffix,
		),
	}
	for _, marker := range fingerprintMarkers {
		index := strings.Index(lines[1], marker)
		if index < 0 {
			continue
		}
		details.Algorithm = strings.TrimSpace(lines[1][:index])
		details.Fingerprint = strings.TrimSpace(
			strings.TrimSuffix(lines[1][index+len(marker):], "."),
		)
		break
	}
	if details.Host == "" || details.Algorithm == "" || details.Fingerprint == "" {
		return hostKeyPromptDetails{}, errors.New("OpenSSH host-key prompt is missing review details")
	}
	return details, nil
}

func validHostKeyReviewLines(lines []string) bool {
	if len(lines) == 0 {
		return true
	}
	if len(lines) == 1 {
		return lines[0] == "This key is not known by any other names."
	}
	if lines[0] != "This host key is known by the following other names/addresses:" {
		return false
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "    ") {
			return false
		}
		reference := strings.TrimSpace(line)
		hostSeparator := strings.Index(reference, ": ")
		if hostSeparator < 0 || strings.TrimSpace(reference[hostSeparator+2:]) == "" {
			return false
		}
		location := reference[:hostSeparator]
		lineColon := strings.LastIndex(location, ":")
		if lineColon < 1 || strings.TrimSpace(location[:lineColon]) == "" {
			return false
		}
		if _, err := strconv.ParseUint(location[lineColon+1:], 10, 64); err != nil {
			return false
		}
	}
	return true
}
