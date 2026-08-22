package ouc

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/emmansun/gmsm/sm2"
)

var ssoConfigMarker = []byte("var ssoConfig =")

type loginSecurityConfig struct {
	SM2 struct {
		Enabled   bool   `json:"enabled"`
		PublicKey string `json:"publicKey"`
	} `json:"sm2"`
}

func parseLoginSecurityConfig(body []byte) (loginSecurityConfig, error) {
	index := bytes.Index(body, ssoConfigMarker)
	if index < 0 {
		return loginSecurityConfig{}, fmt.Errorf("OUC login security config not found")
	}
	reader := bytes.NewReader(body[index+len(ssoConfigMarker):])
	var config loginSecurityConfig
	if err := json.NewDecoder(reader).Decode(&config); err != nil {
		return loginSecurityConfig{}, fmt.Errorf("decode OUC login security config: %w", err)
	}
	if config.SM2.Enabled && strings.TrimSpace(config.SM2.PublicKey) == "" {
		return loginSecurityConfig{}, fmt.Errorf("OUC SM2 public key is missing")
	}
	return config, nil
}

func encryptSM2Password(password string, encodedPublicKey string) (string, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(
		strings.TrimSpace(encodedPublicKey),
	)
	if err != nil {
		return "", fmt.Errorf("decode OUC SM2 public key: %w", err)
	}
	publicKey, err := sm2.NewPublicKey(keyBytes)
	if err != nil {
		return "", fmt.Errorf("parse OUC SM2 public key: %w", err)
	}
	ciphertext, err := sm2.Encrypt(
		rand.Reader,
		publicKey,
		[]byte(password),
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("encrypt OUC password with SM2: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}
