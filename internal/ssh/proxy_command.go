package ssh

import (
	"os"
	"strconv"
	"strings"
)

func proxyCommand(previous Target, connectionArguments []string) string {
	destination, port := previous.CommandDestination()
	arguments := []string{
		"ssh",
		"-F", os.DevNull,
		"-o", "CanonicalizeHostname=no",
	}
	arguments = append(arguments, connectionArguments...)
	arguments = append(arguments,
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPersist=no",
		"-o", "ProxyCommand="+proxyFailureCommand(),
	)
	if port != 0 {
		arguments = append(arguments, "-p", strconv.Itoa(port))
	}
	arguments = append(arguments, "-W", "[%h]:%p", "--", destination)
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = quoteProxyArgument(argument)
	}
	return strings.Join(quoted, " ")
}
