package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Secrets are stored AES-GCM-encrypted with a key file next to the
// database. This protects backups/copies of relay.db, not a fully
// compromised host (nothing can). Values are write-only over HTTP and only
// released to agents for runs that reference them.

var secretNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

func ValidSecretName(name string) bool { return secretNameRe.MatchString(name) }

type SecretStore struct {
	store *Store
	aead  cipher.AEAD
}

func NewSecretStore(store *Store, dataDir string) (*SecretStore, error) {
	keyPath := filepath.Join(dataDir, "secret.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s is corrupt (want 32 bytes, have %d)", keyPath, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if _, err := store.db.Exec(`CREATE TABLE IF NOT EXISTS secrets (
		name TEXT PRIMARY KEY, value BLOB NOT NULL, created_at INTEGER NOT NULL
	)`); err != nil {
		return nil, err
	}
	return &SecretStore{store: store, aead: aead}, nil
}

func (s *SecretStore) Set(name, value string) error {
	if !ValidSecretName(name) {
		return fmt.Errorf(
			"secret name %q must look like an env var: UPPER_SNAKE_CASE "+
				"(e.g. HF_TOKEN). It is injected under exactly that name", name)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := append(nonce, s.aead.Seal(nil, nonce, []byte(value), []byte(name))...)
	_, err := s.store.db.Exec(
		`INSERT INTO secrets(name, value, created_at) VALUES(?,?,?)
		 ON CONFLICT(name) DO UPDATE SET value=excluded.value`,
		name, sealed, time.Now().Unix())
	return err
}

func (s *SecretStore) Get(name string) (string, error) {
	var sealed []byte
	err := s.store.db.QueryRow(`SELECT value FROM secrets WHERE name=?`, name).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("secret %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	ns := s.aead.NonceSize()
	if len(sealed) < ns {
		return "", fmt.Errorf("secret %s is corrupt", name)
	}
	plain, err := s.aead.Open(nil, sealed[:ns], sealed[ns:], []byte(name))
	if err != nil {
		return "", fmt.Errorf("secret %s: decrypt failed (key file changed?)", name)
	}
	return string(plain), nil
}

func (s *SecretStore) Names() ([]string, error) {
	rows, err := s.store.db.Query(`SELECT name FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *SecretStore) Delete(name string) error {
	_, err := s.store.db.Exec(`DELETE FROM secrets WHERE name=?`, name)
	return err
}
