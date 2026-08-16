package ssh

import (
	"errors"
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
	if len(reviewLines) > 1 ||
		len(reviewLines) == 1 && reviewLines[0] != "This key is not known by any other names." {
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
