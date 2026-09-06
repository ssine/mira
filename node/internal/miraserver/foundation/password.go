package foundation

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

const (
	passwordMemory      = 19_456
	passwordIterations  = 2
	passwordParallelism = 1
	passwordSaltBytes   = 16
	passwordHashBytes   = 32
)

func ValidAdminUsername(username string) bool {
	if len(username) < 1 || len(username) > 128 {
		return false
	}
	for _, character := range username {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._@-", character) {
			continue
		}
		return false
	}
	return true
}

// HashPassword emits the same Argon2id PHC format and parameters as the
// released node-argon2 implementation.
func HashPassword(password string) (string, error) {
	length := len(utf16.Encode([]rune(password)))
	if length < 12 || length > 1024 {
		return "", fmt.Errorf("administrator password must contain between 12 and 1024 characters")
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	normalized := norm.NFKC.String(password)
	hash := argon2.IDKey([]byte(normalized), salt, passwordIterations, passwordMemory, passwordParallelism, passwordHashBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, passwordMemory, passwordIterations, passwordParallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(password, encoded string) bool {
	parameters, salt, expected, ok := parseArgon2ID(encoded)
	if !ok {
		return false
	}
	normalized := norm.NFKC.String(password)
	actual := argon2.IDKey([]byte(normalized), salt, parameters.iterations, parameters.memory, parameters.parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

type argon2Parameters struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

func parseArgon2ID(encoded string) (argon2Parameters, []byte, []byte, bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return argon2Parameters{}, nil, nil, false
	}
	values := map[string]uint64{}
	for _, entry := range strings.Split(parts[3], ",") {
		pair := strings.SplitN(entry, "=", 2)
		if len(pair) != 2 || (pair[0] != "m" && pair[0] != "t" && pair[0] != "p") {
			return argon2Parameters{}, nil, nil, false
		}
		value, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil || value == 0 {
			return argon2Parameters{}, nil, nil, false
		}
		if _, duplicate := values[pair[0]]; duplicate {
			return argon2Parameters{}, nil, nil, false
		}
		values[pair[0]] = value
	}
	if len(values) != 3 {
		return argon2Parameters{}, nil, nil, false
	}
	// Bound attacker-controlled parameters while accepting the released hash.
	if values["m"] < 8 || values["m"] > 1_048_576 || values["t"] > 64 || values["p"] > 255 {
		return argon2Parameters{}, nil, nil, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 1024 {
		return argon2Parameters{}, nil, nil, false
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(hash) < 16 || len(hash) > 1024 {
		return argon2Parameters{}, nil, nil, false
	}
	return argon2Parameters{memory: uint32(values["m"]), iterations: uint32(values["t"]), parallelism: uint8(values["p"])}, salt, hash, true
}
