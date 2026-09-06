package supervisorapi

import "regexp"

var versionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validVersion(version string) bool {
	return len(version) > 0 && len(version) <= 128 && versionNamePattern.MatchString(version)
}
