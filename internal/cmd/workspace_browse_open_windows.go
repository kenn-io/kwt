package cmd

import "os"

func openBrowseDirectory(path string) (*os.File, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(".")
}
