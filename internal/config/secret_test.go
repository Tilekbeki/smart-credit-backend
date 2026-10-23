package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDemoSecretRandomnessPersistenceAndPermissions(t *testing.T) {
	secrets := make(map[string]bool)
	for i := 0; i < 8; i++ {
		path := filepath.Join(t.TempDir(), "secrets", "jwt-secret")
		secret, err := resolveJWTSecret("demo", "", path)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(secret)
		if err != nil || len(decoded) != 32 || secrets[secret] {
			t.Fatal("each installation must have its own random 256-bit secret")
		}
		secrets[secret] = true
		fileInfo, err := os.Lstat(path)
		if err != nil || fileInfo.Mode().Perm() != 0600 {
			t.Fatal("secret file must have 0600 permissions")
		}
		dirInfo, err := os.Lstat(filepath.Dir(path))
		if err != nil || dirInfo.Mode().Perm() != 0700 {
			t.Fatal("secret directory must have 0700 permissions")
		}
		reused, err := resolveJWTSecret("demo", "", path)
		if err != nil || reused != secret {
			t.Fatal("restarting must reuse the same secret")
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil || len(entries) != 1 {
			t.Fatal("initialization must remove temporary files")
		}
	}
}

func TestExplicitSecretTakesPrecedenceWithoutFileIO(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"demo", "production"} {
		explicit := strings.Repeat("explicit", 8)
		secret, err := resolveJWTSecret(env, explicit, filepath.Join(blocked, "secret"))
		if err != nil || secret != explicit {
			t.Fatal("explicit secret must take precedence without touching storage")
		}
	}
}

func TestProductionRequiresExplicitSecretEvenWithSavedDemoKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "jwt-secret")
	if _, err := resolveJWTSecret("demo", "", path); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJWTSecret("production", "", path); err == nil {
		t.Fatal("production must not load the saved demo secret")
	}
	missing := filepath.Join(t.TempDir(), "must-not-exist", "jwt-secret")
	if _, err := resolveJWTSecret("production", "", missing); err == nil {
		t.Fatal("production must require explicit configuration")
	}
	if _, err := os.Lstat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatal("production must not create secret storage")
	}
}

func TestInvalidExplicitSecretNeverFallsBackOrLeaksValues(t *testing.T) {
	for _, explicit := range []string{"private-invalid-key", strings.Repeat(" ", 32)} {
		path := filepath.Join(t.TempDir(), "private-path-value", "jwt-secret")
		_, err := resolveJWTSecret("demo", explicit, path)
		if err == nil {
			t.Fatal("invalid explicit secret must fail")
		}
		if strings.Contains(err.Error(), explicit) || strings.Contains(err.Error(), "private-path-value") {
			t.Fatal("errors must not disclose supplied values")
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatal("invalid explicit secret must not initialize storage")
		}
	}
}

func TestRejectInvalidSavedSecretsWithoutReplacingThem(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		mode os.FileMode
	}{
		{name: "short", data: "private-short-value", mode: 0600},
		{name: "empty", mode: 0600},
		{name: "blank", data: strings.Repeat(" ", 40), mode: 0600},
		{name: "too large", data: strings.Repeat("s", maxSecretFileSize+1), mode: 0600},
		{name: "group readable", data: strings.Repeat("s", 40), mode: 0640},
		{name: "world readable", data: strings.Repeat("s", 40), mode: 0644},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private-path-value")
			if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}
			_, err := resolveJWTSecret("demo", "", path)
			if err == nil {
				t.Fatal("invalid saved secret must fail")
			}
			if strings.Contains(err.Error(), "private-path-value") || strings.Contains(err.Error(), "private-short-value") {
				t.Fatal("errors must not expose saved values or paths")
			}
			contents, err := os.ReadFile(path)
			if err != nil || string(contents) != test.data {
				t.Fatal("invalid saved secret must not be overwritten")
			}
		})
	}
}

func TestRejectSymlinksDirectoriesAndInsecureParent(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("s", 40)), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJWTSecret("demo", "", link); err == nil {
		t.Fatal("a saved secret must not be a symlink")
	}
	if _, err := resolveJWTSecret("demo", "", root); err == nil {
		t.Fatal("a directory must not be accepted as a secret")
	}
	insecure := filepath.Join(root, "insecure")
	if err := os.Mkdir(insecure, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(insecure, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJWTSecret("demo", "", filepath.Join(insecure, "secret")); err == nil {
		t.Fatal("a shared directory must not store demo secrets")
	}
	parentLink := filepath.Join(root, "parent-link")
	if err := os.Symlink(root, parentLink); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJWTSecret("demo", "", filepath.Join(parentLink, "secret")); err == nil {
		t.Fatal("the immediate secret parent must not be a symlink")
	}
}

func TestConcurrentDemoSecretInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "jwt-secret")
	const processes = 32
	secrets := make([]string, processes)
	errors := make([]error, processes)
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i := range processes {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			secrets[i], errors[i] = resolveJWTSecret("demo", "", path)
		}()
	}
	close(start)
	workers.Wait()
	for i := range processes {
		if errors[i] != nil {
			t.Fatal(errors[i])
		}
		if secrets[i] != secrets[0] {
			t.Fatal("concurrent initialization must select exactly one persisted secret")
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != secrets[0] {
		t.Fatal("every initializer must use the key that was saved")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("concurrent initializers must clean up their temporary files")
	}
}
