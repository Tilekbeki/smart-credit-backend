package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// LocalStorage keeps private files on one volume. The lifetime lock also makes
// startup recovery safe: another backend cannot publish files in this directory.
type LocalStorage struct {
	root *os.Root
	lock *os.File
}

func NewLocalStorage(path string) (*LocalStorage, error) {
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, ErrStorage
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrStorage
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, ErrStorage
	}
	if err := root.Chmod(".", 0700); err != nil {
		root.Close()
		return nil, ErrStorage
	}
	lock, err := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		root.Close()
		return nil, ErrStorage
	}
	info, err = lock.Stat()
	if err != nil || !info.Mode().IsRegular() || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		root.Close()
		return nil, ErrStorage
	}
	if err := lock.Chmod(0600); err != nil {
		lock.Close()
		root.Close()
		return nil, ErrStorage
	}
	return &LocalStorage{root: root, lock: lock}, nil
}

func (s *LocalStorage) Close() error {
	rootErr := s.root.Close()
	lockErr := s.lock.Close() // Closing the descriptor releases the exclusive lock.
	return errors.Join(rootErr, lockErr)
}

func validKey(key string) bool {
	parsed, err := uuid.Parse(key)
	return err == nil && parsed != uuid.Nil && parsed.String() == key
}

func tempName(key string) string { return ".upload-" + key + ".tmp" }

func validStoredName(name string) bool {
	if validKey(name) {
		return true
	}
	return strings.HasPrefix(name, ".upload-") && strings.HasSuffix(name, ".tmp") && validKey(strings.TrimSuffix(strings.TrimPrefix(name, ".upload-"), ".tmp"))
}

// ValidateFileName checks the metadata before multipart filename normalization
// can hide a path supplied by the client.
func ValidateFileName(name string) error {
	if name == "" || name != strings.TrimSpace(name) || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 255 || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) || filepath.Base(name) != name {
		return ErrInvalidInput
	}
	for _, char := range name {
		if unicode.IsControl(char) {
			return ErrInvalidInput
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *LocalStorage) Stage(ctx context.Context, reader io.Reader, fileName string) (_ Staged, err error) {
	if err := ValidateFileName(fileName); err != nil {
		return Staged{}, err
	}
	key := uuid.NewString()
	name := tempName(key)
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return Staged{}, ErrStorage
	}
	defer func() {
		file.Close()
		if err != nil {
			s.root.Remove(name)
		}
	}()
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx: ctx, reader: reader}, MaxFileSize+1))
	if copyErr != nil || ctx.Err() != nil {
		return Staged{}, ErrStorage
	}
	if size == 0 {
		return Staged{}, ErrInvalidInput
	}
	if size > MaxFileSize {
		return Staged{}, ErrTooLarge
	}
	if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
		return Staged{}, ErrStorage
	}
	sniff := make([]byte, 512)
	n, readErr := file.Read(sniff)
	if readErr != nil && readErr != io.EOF {
		return Staged{}, ErrStorage
	}
	contentType := http.DetectContentType(sniff[:n])
	extension := strings.ToLower(filepath.Ext(fileName))
	valid := (contentType == "application/pdf" && extension == ".pdf") ||
		(contentType == "image/png" && extension == ".png") ||
		(contentType == "image/jpeg" && (extension == ".jpg" || extension == ".jpeg"))
	if !valid {
		return Staged{}, ErrUnsupportedFormat
	}
	if file.Sync() != nil || file.Close() != nil {
		return Staged{}, ErrStorage
	}
	return Staged{Key: key, TempName: name, ContentType: contentType, Size: size, Checksum: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (s *LocalStorage) syncDirectory() error {
	dir, err := s.root.Open(".")
	if err != nil {
		return ErrStorage
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *LocalStorage) Publish(staged Staged) error {
	if !validKey(staged.Key) || staged.TempName != tempName(staged.Key) {
		return ErrStorage
	}
	info, err := s.root.Lstat(staged.TempName)
	if err != nil || !info.Mode().IsRegular() {
		return ErrStorage
	}
	if _, err := s.root.Lstat(staged.Key); !errors.Is(err, os.ErrNotExist) {
		return ErrStorage
	}
	if err := s.root.Rename(staged.TempName, staged.Key); err != nil {
		return ErrStorage
	}
	return s.syncDirectory()
}

func (s *LocalStorage) Remove(name string) error {
	if !validStoredName(name) {
		return ErrStorage
	}
	if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrStorage
	}
	return s.syncDirectory()
}

func (s *LocalStorage) Open(key string) (*os.File, error) {
	if !validKey(key) {
		return nil, ErrStorage
	}
	info, err := s.root.Lstat(key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, ErrStorage
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	file, err := s.root.OpenFile(key, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
			return nil, ErrNotFound
		}
		return nil, ErrStorage
	}
	info, err = file.Stat()
	if err != nil {
		file.Close()
		return nil, ErrStorage
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, ErrNotFound
	}
	return file, nil
}

func (s *LocalStorage) Names() ([]string, error) {
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, ErrStorage
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, ErrStorage
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}
