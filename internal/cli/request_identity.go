package cli

import (
	"errors"
	"regexp"
	"strings"
)

var sdkIdentityPattern = regexp.MustCompile(`^cli/(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// sdkIdentityForVersion preserves the resolved version for User-Agent while
// producing the bounded SemVer identity used by service requests.
func sdkIdentityForVersion(version string) (string, error) {
	value := "cli/" + strings.TrimPrefix(version, "v")
	parts := sdkIdentityPattern.FindStringSubmatch(value)
	if parts == nil {
		return "", errors.New("invalid CLI version for request identity")
	}
	for _, identifier := range strings.Split(parts[4], ".") {
		if len(identifier) > 1 && identifier[0] == '0' && strings.Trim(identifier, "0123456789") == "" {
			return "", errors.New("invalid CLI version for request identity")
		}
	}
	value, _, _ = strings.Cut(value, "+")
	if len(value) > 64 {
		return "", errors.New("CLI request identity exceeds 64 bytes")
	}
	return value, nil
}
