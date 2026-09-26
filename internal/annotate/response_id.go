package annotate

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

const localResponsePrefix = "resp_torana_"
const localResponsePurpose = "torana/local-response/v1"

// EncodeLocalResponseID gives a Responses local reply an ID the harness can
// cite, while carrying the last real provider response ID through that turn.
func EncodeLocalResponseID(signer Signer, providerID string) (string, error) {
	if len(providerID) > 2048 {
		return "", errors.New("provider response ID is too long")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	nonceHex := hex.EncodeToString(nonce[:])
	mac, err := signer.MAC(localResponsePurpose, providerID+"\x00"+nonceHex)
	if err != nil {
		return "", err
	}
	if len(mac) < 12 {
		return "", errors.New("response ID MAC is too short")
	}
	return localResponsePrefix + base64.RawURLEncoding.EncodeToString([]byte(providerID)) + "." + nonceHex + "." + hex.EncodeToString(mac[:12]), nil
}

// DecodeLocalResponseID recognizes only Torana's signed local reply IDs.
// Ordinary provider IDs pass through unchanged; an invalid Torana-shaped ID
// is an error, never sent upstream as if it belonged to the provider.
func DecodeLocalResponseID(signer Signer, id string) (providerID string, recognized bool, err error) {
	if !strings.HasPrefix(id, localResponsePrefix) {
		return id, false, nil
	}
	parts := strings.Split(strings.TrimPrefix(id, localResponsePrefix), ".")
	if len(parts) != 3 || len(parts[1]) != 16 || len(parts[2]) != 24 {
		return "", true, errors.New("invalid local response ID")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(decoded) > 2048 {
		return "", true, errors.New("invalid local response ID")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", true, errors.New("invalid local response ID")
	}
	provided, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", true, errors.New("invalid local response ID")
	}
	mac, err := signer.MAC(localResponsePurpose, string(decoded)+"\x00"+parts[1])
	if err != nil {
		return "", true, err
	}
	if len(mac) < len(provided) || subtle.ConstantTimeCompare(provided, mac[:len(provided)]) != 1 {
		return "", true, errors.New("invalid local response ID signature")
	}
	return string(decoded), true, nil
}
