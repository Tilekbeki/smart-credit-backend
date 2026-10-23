package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxSecretFileSize = 4096

func validJWTSecret(secret string) bool {
	return len(secret) >= 32 && strings.TrimSpace(secret) != ""
}

// resolveJWTSecret never includes supplied values or underlying filesystem
// errors in its errors: paths and secrets can contain sensitive data.
func resolveJWTSecret(appEnv, explicitSecret, secretFile string) (string, error) {
	if explicitSecret != "" {
		if !validJWTSecret(explicitSecret) {
			return "", errors.New("JWT_SECRET: must contain at least 32 bytes")
		}
		return explicitSecret, nil
	}
	if appEnv != "demo" {
		return "", errors.New("JWT_SECRET: is required in production and must contain at least 32 bytes")
	}

	directory := filepath.Dir(secretFile)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", errors.New("JWT_SECRET_FILE: cannot create private secret directory")
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0077 != 0 {
		return "", errors.New("JWT_SECRET_FILE: parent must be a private directory without symlinks")
	}
	secret, err := readJWTSecret(secretFile)
	if err == nil {
		return secret, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	var randomBytes [32]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", errors.New("JWT_SECRET_FILE: cannot generate random demo secret")
	}
	secret = base64.RawURLEncoding.EncodeToString(randomBytes[:])
	temp, err := os.CreateTemp(directory, ".jwt-secret-*")
	if err != nil {
		return "", errors.New("JWT_SECRET_FILE: cannot create private temporary secret")
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err := temp.Chmod(0600); err != nil {
		return "", errors.New("JWT_SECRET_FILE: cannot restrict secret permissions")
	}
	if _, err := temp.WriteString(secret); err != nil {
		return "", errors.New("JWT_SECRET_FILE: cannot persist demo secret")
	}
	if err := temp.Sync(); err != nil {
		return "", errors.New("JWT_SECRET_FILE: cannot persist demo secret")
	}
	if err := temp.Close(); err != nil {
		return "", errors.New("JWT_SECRET_FILE: cannot persist demo secret")
	}
	// A hard link publishes only the complete file and never overwrites a
	// winner from another process. Every process then uses the same key.
	if err := os.Link(temp.Name(), secretFile); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readJWTSecret(secretFile)
		}
		return "", errors.New("JWT_SECRET_FILE: cannot install persistent demo secret")
	}
	return secret, nil
}

func readJWTSecret(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", os.ErrNotExist
	}
	if err != nil {
		return "", errors.New("JWT_SECRET_FILE: cannot inspect saved demo secret")
	}
	if !info.Mode().IsRegular() || (info.Mode().Perm() != 0600 && info.Mode().Perm() != 0400) {
		return "", errors.New("JWT_SECRET_FILE: saved secret must be a regular private file (0600 or 0400)")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("JWT_SECRET_FILE: cannot read saved demo secret")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return "", errors.New("JWT_SECRET_FILE: saved demo secret changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxSecretFileSize+1))
	if err != nil || len(contents) > maxSecretFileSize || !validJWTSecret(string(contents)) {
		return "", errors.New("JWT_SECRET_FILE: saved demo secret is invalid; refusing to replace it")
	}
	return string(contents), nil
}
