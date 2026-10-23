package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPDF() []byte { return []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n") }

func testImage(t *testing.T, kind string) []byte {
	t.Helper()
	var output bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var err error
	if kind == "png" {
		err = png.Encode(&output, img)
	} else {
		err = jpeg.Encode(&output, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func localTestStorage(t *testing.T) (*LocalStorage, string) {
	t.Helper()
	path := t.TempDir()
	storage, err := NewLocalStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	return storage, path
}

func assertNoDocumentFiles(t *testing.T, storage *LocalStorage) {
	t.Helper()
	names, err := storage.Names()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name != ".lock" {
			t.Fatalf("unexpected retained file: %s", name)
		}
	}
}

func TestLocalStorageFormatsAndPrivateNames(t *testing.T) {
	for _, example := range []struct {
		name, kind string
		data       []byte
	}{
		{"выписка.PDF", "application/pdf", testPDF()},
		{"invoice.png", "image/png", testImage(t, "png")},
		{"invoice.JPEG", "image/jpeg", testImage(t, "jpeg")},
	} {
		t.Run(example.name, func(t *testing.T) {
			storage, path := localTestStorage(t)
			staged, err := storage.Stage(t.Context(), bytes.NewReader(example.data), example.name)
			if err != nil {
				t.Fatal(err)
			}
			if !validKey(staged.Key) || staged.TempName != tempName(staged.Key) || staged.ContentType != example.kind || staged.Size != int64(len(example.data)) {
				t.Fatalf("incorrect staged metadata: %+v", staged)
			}
			digest := sha256.Sum256(example.data)
			if staged.Checksum != hex.EncodeToString(digest[:]) {
				t.Fatal("incorrect checksum")
			}
			if _, err := storage.Open(staged.Key); !errors.Is(err, ErrNotFound) {
				t.Fatal("unpublished file must not be readable")
			}
			if err := storage.Publish(staged); err != nil {
				t.Fatal(err)
			}
			file, err := storage.Open(staged.Key)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			actual, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(actual, example.data) {
				t.Fatal("file content changed")
			}
			info, err := file.Stat()
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("file is not private")
			}
			info, err = os.Stat(path)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatal("storage directory is not private")
			}
			if _, err := os.Stat(filepath.Join(path, example.name)); !os.IsNotExist(err) {
				t.Fatal("original filename used on filesystem")
			}
			if err := storage.Remove(staged.Key); err != nil {
				t.Fatal(err)
			}
			assertNoDocumentFiles(t, storage)
		})
	}
}

func TestLocalStorageValidationAndCleanup(t *testing.T) {
	tooLarge := append(testPDF(), make([]byte, int(MaxFileSize)+1-len(testPDF()))...)
	for _, example := range []struct {
		name, filename string
		data           []byte
		want           error
	}{
		{"empty", "statement.pdf", nil, ErrInvalidInput},
		{"large", "statement.pdf", tooLarge, ErrTooLarge},
		{"unsupported", "statement.txt", []byte("not a document"), ErrUnsupportedFormat},
		{"extension mismatch", "statement.png", testPDF(), ErrUnsupportedFormat},
		{"fake client type", "statement.pdf", []byte("not PDF despite its extension"), ErrUnsupportedFormat},
		{"missing extension", "statement", testPDF(), ErrUnsupportedFormat},
		{"path traversal", "../statement.pdf", testPDF(), ErrInvalidInput},
		{"windows traversal", `..\statement.pdf`, testPDF(), ErrInvalidInput},
		{"absolute path", "/statement.pdf", testPDF(), ErrInvalidInput},
		{"control", "statement\n.pdf", testPDF(), ErrInvalidInput},
		{"nul", "statement\x00.pdf", testPDF(), ErrInvalidInput},
		{"name too long", strings.Repeat("я", 252) + ".pdf", testPDF(), ErrInvalidInput},
		{"empty name", "", testPDF(), ErrInvalidInput},
	} {
		t.Run(example.name, func(t *testing.T) {
			storage, _ := localTestStorage(t)
			_, err := storage.Stage(t.Context(), bytes.NewReader(example.data), example.filename)
			if !errors.Is(err, example.want) {
				t.Fatalf("got %v, want %v", err, example.want)
			}
			assertNoDocumentFiles(t, storage)
		})
	}
	storage, _ := localTestStorage(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := storage.Stage(ctx, bytes.NewReader(testPDF()), "statement.pdf"); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	assertNoDocumentFiles(t, storage)
	if _, err := storage.Stage(t.Context(), brokenReader{}, "statement.pdf"); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	assertNoDocumentFiles(t, storage)
	max := append(testPDF(), make([]byte, int(MaxFileSize)-len(testPDF()))...)
	staged, err := storage.Stage(t.Context(), bytes.NewReader(max), "max.pdf")
	if err != nil || staged.Size != MaxFileSize {
		t.Fatalf("exact size limit: %v", err)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestLocalStorageLockRestartAndSymlinks(t *testing.T) {
	path := t.TempDir()
	first, err := NewLocalStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := first.Stage(t.Context(), bytes.NewReader(testPDF()), "statement.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Publish(staged); err != nil {
		t.Fatal(err)
	}
	if second, err := NewLocalStorage(path); !errors.Is(err, ErrStorage) {
		second.Close()
		t.Fatal("second writer acquired the volume")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewLocalStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	file, err := restarted.Open(staged.Key)
	if err != nil {
		t.Fatal("published file did not survive restart", err)
	}
	file.Close()
	for _, key := range []string{"../secret", "/etc/passwd", ".lock", "statement.pdf"} {
		if file, err := restarted.Open(key); !errors.Is(err, ErrStorage) {
			file.Close()
			t.Fatal("unsafe open accepted", key)
		}
		if err := restarted.Remove(key); !errors.Is(err, ErrStorage) {
			t.Fatal("unsafe remove accepted", key)
		}
	}
	key := "a8107c92-b136-4855-93d9-821b777b7f11"
	if err := os.Symlink("/etc/passwd", filepath.Join(path, key)); err != nil {
		t.Fatal(err)
	}
	if file, err := restarted.Open(key); !errors.Is(err, ErrNotFound) {
		file.Close()
		t.Fatal("symlink was opened")
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(path, tempName(key))); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Publish(Staged{Key: key, TempName: tempName(key)}); !errors.Is(err, ErrStorage) {
		t.Fatal("symlink was published")
	}
	if err := restarted.Remove(key); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(path, linked); err != nil {
		t.Fatal(err)
	}
	if result, err := NewLocalStorage(linked); !errors.Is(err, ErrStorage) {
		result.Close()
		t.Fatal("symlink root accepted")
	}
}
