package ouc

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/emmansun/gmsm/sm2"
)

func TestParseLoginSecurityConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		body        string
		wantEnabled bool
		wantError   bool
	}{
		{
			name: "SM2 disabled",
			body: `<script>
				var ssoConfig = {"sm2":{"enabled":false,"publicKey":""},"challenge":"masked"};
			</script>`,
		},
		{
			name: "SM2 enabled",
			body: `<script>
				var ssoConfig = {"other":{"nested":true},"sm2":{"enabled":true,"publicKey":"public-key"}};
			</script>`,
			wantEnabled: true,
		},
		{
			name:      "missing config",
			body:      `<html><form id="loginForm"></form></html>`,
			wantError: true,
		},
		{
			name: "enabled without key",
			body: `<script>
				var ssoConfig = {"sm2":{"enabled":true,"publicKey":""}};
			</script>`,
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, err := parseLoginSecurityConfig([]byte(test.body))
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%v", err, test.wantError)
			}
			if err == nil && config.SM2.Enabled != test.wantEnabled {
				t.Fatalf(
					"SM2 enabled=%v want=%v",
					config.SM2.Enabled,
					test.wantEnabled,
				)
			}
		})
	}
}

func TestEncryptSM2PasswordProducesDecryptableBase64C1C3C2(t *testing.T) {
	t.Parallel()
	privateKey, err := sm2.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdhPublicKey, err := sm2.PublicKeyToECDH(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := ecdhPublicKey.Bytes()
	encrypted, err := encryptSM2Password(
		"not-a-real-password",
		base64.StdEncoding.EncodeToString(publicKey),
	)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := privateKey.Decrypt(rand.Reader, ciphertext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(plaintext); got != "not-a-real-password" {
		t.Fatalf("plaintext=%q", got)
	}
	if strings.Contains(encrypted, "not-a-real-password") {
		t.Fatal("ciphertext contains plaintext")
	}
}
