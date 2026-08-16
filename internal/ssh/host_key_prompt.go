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
	)
	fingerprintMarkers := []string{
		" key fingerprint is: ",
		" key fingerprint is ",
	}

	var details hostKeyPromptDetails
	for _, rawLine := range strings.Split(message, "\n") {
		line := strings.TrimSpace(rawLine)
		if details.Host == "" {
			start := strings.Index(line, hostPrefix)
			if start >= 0 {
				start += len(hostPrefix)
				if end := strings.Index(line[start:], hostSuffix); end > 0 {
					details.Host = line[start : start+end]
				}
			}
		}
		if details.Algorithm == "" {
			for _, marker := range fingerprintMarkers {
				index := strings.Index(line, marker)
				if index < 0 {
					continue
				}
				details.Algorithm = strings.TrimSpace(line[:index])
				details.Fingerprint = strings.TrimSuffix(
					strings.TrimSpace(line[index+len(marker):]),
					".",
				)
				break
			}
		}
	}
	if details.Host == "" || details.Algorithm == "" || details.Fingerprint == "" {
		return hostKeyPromptDetails{}, errors.New("OpenSSH host-key prompt is missing review details")
	}
	return details, nil
}
