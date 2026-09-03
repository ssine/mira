package node

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
)

type enrollmentState struct {
	ID               string `json:"id,omitempty"`
	VerificationCode string `json:"verificationCode,omitempty"`
	Status           string `json:"status,omitempty"`
	ExpiresAt        string `json:"expiresAt,omitempty"`
}

type persistedNodeState struct {
	Version      int             `json:"version"`
	ServerURL    string          `json:"serverUrl"`
	NodeID       string          `json:"nodeId,omitempty"`
	NodeKey      string          `json:"nodeKey"`
	CredentialID string          `json:"credentialId"`
	Token        string          `json:"token"`
	ApprovedAt   string          `json:"approvedAt,omitempty"`
	Enrollment   enrollmentState `json:"enrollment,omitempty"`
}

var nodeTokenPattern = regexp.MustCompile(`^mira_node_([0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})_([A-Za-z0-9_-]{43})$`)

func DefaultIdentityFile() (string, error) {
	if value := os.Getenv("MIRA_IDENTITY_FILE"); value != "" {
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("MIRA_IDENTITY_FILE must be an absolute path")
		}
		return value, nil
	}
	if runtime.GOOS == "windows" && os.Getenv("LOCALAPPDATA") != "" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "Mira", "identity.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine user home directory: %w", err)
	}
	return filepath.Join(home, ".config", "mira", "identity.json"), nil
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func newNodeCredential() (string, string, error) {
	credentialID, err := randomUUID()
	if err != nil {
		return "", "", fmt.Errorf("generate credential ID: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return "", "", fmt.Errorf("generate credential secret: %w", err)
	}
	return credentialID, "mira_node_" + credentialID + "_" + base64.RawURLEncoding.EncodeToString(secret), nil
}

func validateNodeToken(token, credentialID string) error {
	match := nodeTokenPattern.FindStringSubmatch(token)
	if match == nil || match[1] != credentialID {
		return fmt.Errorf("identity contains an invalid Node credential")
	}
	return nil
}

func loadIdentity(path string) (*persistedNodeState, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state persistedNodeState
	if err := json.Unmarshal(contents, &state); err != nil {
		return nil, fmt.Errorf("parse Mira identity: %w", err)
	}
	if state.Version != 1 {
		return nil, fmt.Errorf("unsupported Mira identity version: %d", state.Version)
	}
	if state.ServerURL == "" || state.NodeKey == "" || state.CredentialID == "" {
		return nil, fmt.Errorf("Mira identity is incomplete")
	}
	if err := validateNodeToken(state.Token, state.CredentialID); err != nil {
		return nil, err
	}
	return &state, nil
}

func loadOrCreateNodeState(configuration config, identity nodeIdentity) (*persistedNodeState, error) {
	state, err := loadIdentity(configuration.IdentityFile)
	if err == nil {
		if state.ServerURL != configuration.ServerURL || state.NodeKey != identity.NodeKey {
			return nil, fmt.Errorf("Mira identity belongs to %s / %s; use a different MIRA_IDENTITY_FILE", state.ServerURL, state.NodeKey)
		}
		return state, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read Mira identity: %w", err)
	}
	credentialID, token, err := newNodeCredential()
	if err != nil {
		return nil, err
	}
	state = &persistedNodeState{
		Version: 1, ServerURL: configuration.ServerURL, NodeKey: identity.NodeKey,
		CredentialID: credentialID, Token: token, Enrollment: enrollmentState{Status: "new"},
	}
	if err := state.save(configuration.IdentityFile); err != nil {
		return nil, err
	}
	return state, nil
}

func (state *persistedNodeState) resetCredential() error {
	credentialID, token, err := newNodeCredential()
	if err != nil {
		return err
	}
	state.NodeID, state.ApprovedAt = "", ""
	state.CredentialID, state.Token = credentialID, token
	state.Enrollment = enrollmentState{Status: "new"}
	return nil
}

func (state *persistedNodeState) credentialSecretHash() (string, error) {
	match := nodeTokenPattern.FindStringSubmatch(state.Token)
	if match == nil || match[1] != state.CredentialID {
		return "", fmt.Errorf("identity contains an invalid Node credential")
	}
	secret, err := base64.RawURLEncoding.DecodeString(match[2])
	if err != nil || len(secret) != 32 {
		return "", fmt.Errorf("identity contains an invalid Node secret")
	}
	hash := sha256.Sum256(secret)
	return hex.EncodeToString(hash[:]), nil
}

func (state *persistedNodeState) fingerprint() string {
	hash, err := state.credentialSecretHash()
	if err != nil {
		return "invalid"
	}
	return hash[0:4] + "-" + hash[4:8] + "-" + hash[8:12] + "-" + hash[12:16]
}

func (state *persistedNodeState) markApproved(nodeID string, approvedAt string) {
	state.NodeID = nodeID
	if approvedAt == "" {
		approvedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	state.ApprovedAt = approvedAt
	state.Enrollment.Status = "approved"
}

func (state *persistedNodeState) save(path string) error {
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Mira identity: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create Mira identity directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".identity-*")
	if err != nil {
		return fmt.Errorf("create temporary Mira identity: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := protectIdentityFile(temporary); err != nil {
		temporary.Close()
		return fmt.Errorf("protect Mira identity: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return fmt.Errorf("write Mira identity: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync Mira identity: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Mira identity: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install Mira identity: %w", err)
	}
	return nil
}
