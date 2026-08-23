package status

import (
	"bytes"
	"fmt"
)

func parseChangePorcelainV2(input []byte) ([]FileChange, error) {
	changes := make([]FileChange, 0)
	for len(input) > 0 {
		separator := bytes.IndexByte(input, 0)
		if separator < 0 {
			return nil, fmt.Errorf("porcelain v2 record is not NUL terminated")
		}
		record := input[:separator]
		input = input[separator+1:]
		if len(record) == 0 {
			return nil, fmt.Errorf("porcelain v2 contains an empty record")
		}

		var change FileChange
		var err error
		switch record[0] {
		case '2':
			originalSeparator := bytes.IndexByte(input, 0)
			if originalSeparator < 0 {
				return nil, fmt.Errorf(
					"porcelain v2 rename or copy record has no original path",
				)
			}
			originalPath := input[:originalSeparator]
			input = input[originalSeparator+1:]
			change, err = parsePorcelainV2RenameOrCopy(record, originalPath)
		case '1', '?', 'u':
			change, err = parsePorcelainV2Record(record)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func parsePorcelainV2RenameOrCopy(
	record, originalPath []byte,
) (FileChange, error) {
	fields := bytes.SplitN(record, []byte{' '}, 10)
	if len(fields) != 10 || string(fields[0]) != "2" ||
		len(fields[1]) != 2 || !isPorcelainV2Submodule(fields[2]) ||
		!arePorcelainV2Modes(fields[3:6]) ||
		!arePorcelainV2ObjectIDs(fields[6:8]) || len(fields[9]) == 0 ||
		len(originalPath) == 0 || !isPorcelainV2RenameOrCopyScore(
		fields[8],
		fields[1],
	) {
		return FileChange{}, fmt.Errorf("malformed porcelain v2 rename or copy record")
	}
	index, err := parsePorcelainV2State(fields[1][0])
	if err != nil {
		return FileChange{}, err
	}
	worktree, err := parsePorcelainV2State(fields[1][1])
	if err != nil {
		return FileChange{}, err
	}
	return FileChange{
		Path:         string(fields[9]),
		OriginalPath: string(originalPath),
		Index:        index,
		Worktree:     worktree,
	}, nil
}

func isPorcelainV2RenameOrCopyScore(score, state []byte) bool {
	if len(score) < 2 || len(score) > 4 || len(state) != 2 ||
		(score[0] != 'R' && score[0] != 'C') ||
		(state[0] != score[0] && state[1] != score[0]) {
		return false
	}
	value := 0
	for _, digit := range score[1:] {
		if digit < '0' || digit > '9' {
			return false
		}
		value = value*10 + int(digit-'0')
	}
	return value <= 100
}

func parsePorcelainV2Record(record []byte) (FileChange, error) {
	switch record[0] {
	case '1':
		fields := bytes.SplitN(record, []byte{' '}, 9)
		if len(fields) != 9 || string(fields[0]) != "1" ||
			len(fields[1]) != 2 || !isPorcelainV2Submodule(fields[2]) ||
			!arePorcelainV2Modes(fields[3:6]) ||
			!arePorcelainV2ObjectIDs(fields[6:8]) || len(fields[8]) == 0 {
			return FileChange{}, fmt.Errorf("malformed porcelain v2 ordinary record")
		}
		index, err := parsePorcelainV2State(fields[1][0])
		if err != nil {
			return FileChange{}, err
		}
		worktree, err := parsePorcelainV2State(fields[1][1])
		if err != nil {
			return FileChange{}, err
		}
		return FileChange{
			Path:     string(fields[8]),
			Index:    index,
			Worktree: worktree,
		}, nil
	case '?':
		if len(record) < 3 || record[1] != ' ' {
			return FileChange{}, fmt.Errorf("malformed porcelain v2 untracked record")
		}
		return FileChange{
			Path:     string(record[2:]),
			Worktree: FileStateUntracked,
		}, nil
	case 'u':
		fields := bytes.SplitN(record, []byte{' '}, 11)
		if len(fields) != 11 || string(fields[0]) != "u" ||
			!isPorcelainV2UnmergedCode(fields[1]) ||
			!isPorcelainV2Submodule(fields[2]) ||
			!arePorcelainV2Modes(fields[3:7]) ||
			!arePorcelainV2ObjectIDs(fields[7:10]) ||
			len(fields[10]) == 0 {
			return FileChange{}, fmt.Errorf("malformed porcelain v2 unmerged record")
		}
		return FileChange{
			Path:     string(fields[10]),
			Index:    FileStateConflicted,
			Worktree: FileStateConflicted,
		}, nil
	default:
		return FileChange{}, fmt.Errorf(
			"unknown porcelain v2 record type %q",
			record[0],
		)
	}
}

func isPorcelainV2Submodule(field []byte) bool {
	if bytes.Equal(field, []byte("N...")) {
		return true
	}
	return len(field) == 4 && field[0] == 'S' &&
		(field[1] == '.' || field[1] == 'C') &&
		(field[2] == '.' || field[2] == 'M') &&
		(field[3] == '.' || field[3] == 'U')
}

func arePorcelainV2Modes(fields [][]byte) bool {
	for _, field := range fields {
		if len(field) != 6 {
			return false
		}
		for _, digit := range field {
			if digit < '0' || digit > '7' {
				return false
			}
		}
	}
	return true
}

func arePorcelainV2ObjectIDs(fields [][]byte) bool {
	for _, field := range fields {
		if len(field) != 40 && len(field) != 64 {
			return false
		}
		for _, digit := range field {
			if (digit < '0' || digit > '9') &&
				(digit < 'a' || digit > 'f') &&
				(digit < 'A' || digit > 'F') {
				return false
			}
		}
	}
	return true
}

func isPorcelainV2UnmergedCode(code []byte) bool {
	switch string(code) {
	case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
		return true
	default:
		return false
	}
}

func parsePorcelainV2State(code byte) (FileState, error) {
	switch code {
	case '.':
		return "", nil
	case 'M', 'T':
		return FileStateModified, nil
	case 'A':
		return FileStateAdded, nil
	case 'D':
		return FileStateDeleted, nil
	case 'R':
		return FileStateRenamed, nil
	case 'C':
		return FileStateCopied, nil
	default:
		return "", fmt.Errorf("unknown porcelain v2 state %q", code)
	}
}
