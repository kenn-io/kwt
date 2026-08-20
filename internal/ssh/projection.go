package ssh

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"strconv"
	"strings"

	"go.kenn.io/kit/openssh"
)

const projectionPolicyV1 = "kwt.openssh.projection.v1"

const ProjectionPolicyV1 = projectionPolicyV1

type projection struct {
	PolicyVersion string
	Arguments     []string
	PrivateConfig []string
	ForwardAgent  bool
}

type projectionOption struct {
	name    string
	openSSH string
	private bool
	session bool
}

var projectionOptionsV1 = []projectionOption{
	{name: "userknownhostsfile", openSSH: "UserKnownHostsFile"},
	{name: "globalknownhostsfile", openSSH: "GlobalKnownHostsFile"},
	{name: "knownhostscommand", openSSH: "KnownHostsCommand"},
	{name: "revokedhostkeys", openSSH: "RevokedHostKeys"},
	{name: "hostkeyalgorithms", openSSH: "HostKeyAlgorithms"},
	{name: "kexalgorithms", openSSH: "KexAlgorithms"},
	{name: "ciphers", openSSH: "Ciphers"},
	{name: "macs", openSSH: "MACs"},
	{name: "requiredrsasize", openSSH: "RequiredRSASize"},
	{name: "casignaturealgorithms", openSSH: "CASignatureAlgorithms"},
	{name: "checkhostip", openSSH: "CheckHostIP"},
	{name: "hashknownhosts", openSSH: "HashKnownHosts"},
	{name: "verifyhostkeydns", openSSH: "VerifyHostKeyDNS"},
	{name: "visualhostkey", openSSH: "VisualHostKey"},
	{name: "fingerprinthash", openSSH: "FingerprintHash"},
	{name: "addressfamily", openSSH: "AddressFamily"},
	{name: "bindaddress", openSSH: "BindAddress"},
	{name: "bindinterface", openSSH: "BindInterface"},
	{name: "addkeystoagent", openSSH: "AddKeysToAgent"},
	{name: "certificatefile", openSSH: "CertificateFile", private: true},
	{name: "enablesshkeysign", openSSH: "EnableSSHKeysign"},
	{name: "forwardagent", openSSH: "ForwardAgent", session: true},
	{name: "gssapiauthentication", openSSH: "GSSAPIAuthentication"},
	{name: "gssapidelegatecredentials", openSSH: "GSSAPIDelegateCredentials"},
	{name: "hostbasedacceptedalgorithms", openSSH: "HostbasedAcceptedAlgorithms"},
	{name: "hostbasedauthentication", openSSH: "HostbasedAuthentication"},
	{name: "identitiesonly", openSSH: "IdentitiesOnly"},
	{name: "identityagent", openSSH: "IdentityAgent", private: true},
	{name: "identityfile", openSSH: "IdentityFile", private: true},
	{name: "kbdinteractiveauthentication", openSSH: "KbdInteractiveAuthentication"},
	{name: "passwordauthentication", openSSH: "PasswordAuthentication"},
	{name: "pkcs11provider", openSSH: "PKCS11Provider", private: true},
	{name: "preferredauthentications", openSSH: "PreferredAuthentications"},
	{name: "pubkeyacceptedalgorithms", openSSH: "PubkeyAcceptedAlgorithms"},
	{name: "pubkeyauthentication", openSSH: "PubkeyAuthentication"},
	{name: "securitykeyprovider", openSSH: "SecurityKeyProvider", private: true},
	{name: "usekeychain", openSSH: "UseKeychain"},
	{name: "escapechar", openSSH: "EscapeChar", session: true},
	{name: "sendenv", openSSH: "SendEnv", session: true},
	{name: "setenv", openSSH: "SetEnv", private: true, session: true},
}

func multiplexedSessionProjection(projected ExecutionProjection) ExecutionProjection {
	result := ExecutionProjection{Arguments: []string{"-F", os.DevNull}}
	for index := 0; index+1 < len(projected.Arguments); index++ {
		if projected.Arguments[index] != "-o" {
			continue
		}
		name, _, found := strings.Cut(projected.Arguments[index+1], "=")
		if found && isSessionProjectionOption(name) {
			result.Arguments = append(
				result.Arguments, projected.Arguments[index], projected.Arguments[index+1],
			)
		}
		index++
	}
	for _, line := range projected.PrivateConfig {
		name, _, _ := strings.Cut(line, " ")
		if isSessionProjectionOption(name) {
			result.PrivateConfig = append(result.PrivateConfig, line)
		}
	}
	return result
}

func isSessionProjectionOption(name string) bool {
	for _, option := range projectionOptionsV1 {
		if option.session && strings.EqualFold(option.openSSH, name) {
			return true
		}
	}
	return false
}

func projectConfig(
	config openssh.EffectiveConfig,
	configuredIdentities []string,
) (projection, error) {
	result := projection{
		PolicyVersion: projectionPolicyV1,
		Arguments: []string{
			"-F", os.DevNull,
			"-o", "CanonicalizeHostname=no",
		},
	}
	if config.Hostname != "" {
		result.Arguments = append(result.Arguments, "-o", "HostName="+config.Hostname)
	}
	if config.User != "" {
		result.Arguments = append(result.Arguments, "-o", "User="+config.User)
	}
	if config.Port != 0 {
		result.Arguments = append(result.Arguments, "-p", strconv.Itoa(config.Port))
	}
	if config.HostKeyAlias != "" {
		result.Arguments = append(result.Arguments, "-o", "HostKeyAlias="+config.HostKeyAlias)
	}

	values := make(map[string][]string)
	for _, option := range config.Options {
		name := strings.ToLower(option.Name)
		values[name] = append(values[name], option.Value)
	}
	if configuredIdentities != nil {
		values["identityfile"] = configuredIdentities
	}
	for _, option := range projectionOptionsV1 {
		for _, value := range values[option.name] {
			if option.name == "forwardagent" && !strings.EqualFold(value, "no") {
				result.ForwardAgent = true
			}
			if option.private {
				line, err := privateConfigLine(option.openSSH, value)
				if err != nil {
					return projection{}, err
				}
				result.PrivateConfig = append(result.PrivateConfig, line)
				continue
			}
			result.Arguments = append(result.Arguments, "-o", option.openSSH+"="+value)
		}
	}
	return result, nil
}

func privateConfigLine(name, value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%s contains an unsafe configuration value", name)
	}
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return name + ` "` + replacer.Replace(value) + `"`, nil
}

func routeIdentity(
	policy string,
	route openssh.Route,
	identityFiles map[openssh.Target][]string,
) string {
	hash := sha256.New()
	writeIdentityPart(hash, openssh.RouteIdentity(policy, route))
	for _, hop := range route {
		values, present := identityFiles[hop.Target]
		if !present {
			_, _ = hash.Write([]byte{0})
			continue
		}
		_, _ = hash.Write([]byte{1})
		var count [8]byte
		binary.BigEndian.PutUint64(count[:], uint64(len(values)))
		_, _ = hash.Write(count[:])
		for _, value := range values {
			writeIdentityPart(hash, value)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeIdentityPart(hash hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write([]byte(value))
}
