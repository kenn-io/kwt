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
	remaining, valid := consumeHostKeyRandomart(lines)
	if !valid {
		return false
	}
	lines = remaining
	if len(lines) > 0 {
		switch lines[0] {
		case "Matching host key fingerprint found in DNS.",
			"No matching host key fingerprint found in DNS.":
			lines = lines[1:]
		}
	}
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

func consumeHostKeyRandomart(lines []string) ([]string, bool) {
	const (
		lineWidth  = 19
		fieldLines = 9
		blockLines = fieldLines + 2
		fieldChars = " .o+=*BOX@%&#/^SE"
	)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "+") {
		return lines, true
	}
	if len(lines) < blockLines ||
		!validHostKeyRandomartFrame(lines[0], lineWidth) ||
		!validHostKeyRandomartFrame(lines[blockLines-1], lineWidth) {
		return nil, false
	}
	for _, line := range lines[1 : blockLines-1] {
		if len(line) != lineWidth || line[0] != '|' || line[len(line)-1] != '|' {
			return nil, false
		}
		for _, character := range line[1 : len(line)-1] {
			if !strings.ContainsRune(fieldChars, character) {
				return nil, false
			}
		}
	}
	return lines[blockLines:], true
}

func validHostKeyRandomartFrame(line string, width int) bool {
	if len(line) != width || line[0] != '+' || line[len(line)-1] != '+' {
		return false
	}
	labelStart := strings.IndexByte(line, '[')
	labelEnd := strings.IndexByte(line, ']')
	if labelStart < 1 || labelEnd <= labelStart+1 || labelEnd >= len(line)-1 {
		return false
	}
	for _, character := range line[1:labelStart] + line[labelEnd+1:len(line)-1] {
		if character != '-' {
			return false
		}
	}
	for _, character := range line[labelStart+1 : labelEnd] {
		if character < 0x20 || character > 0x7e || character == '[' || character == ']' {
			return false
		}
	}
	return true
}
