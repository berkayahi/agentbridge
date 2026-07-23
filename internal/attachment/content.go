package attachment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

var (
	ErrUnsafePath     = errors.New("attachment: unsafe storage path")
	ErrContentChanged = errors.New("attachment: stored content changed")
)

// ContentReader serves verified attachment bytes without permitting access
// outside the configured attachment root.
type ContentReader struct {
	root    string
	maxSize int64
}

func NewContentReader(root string, maxSize int64) (*ContentReader, error) {
	if strings.TrimSpace(root) == "" || maxSize < 1 {
		return nil, errors.New("attachment: invalid content reader configuration")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve attachment root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect attachment root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("attachment: content root is not a directory")
	}
	return &ContentReader{root: filepath.Clean(absRoot), maxSize: maxSize}, nil
}

func (r *ContentReader) Read(ctx context.Context, value workmodel.Attachment) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if value.SizeBytes < 0 || value.SizeBytes > r.maxSize {
		return nil, ErrTooLarge
	}
	expected, err := hex.DecodeString(value.SHA256)
	if err != nil || len(expected) != sha256.Size {
		return nil, ErrContentChanged
	}
	rel, err := r.relativePath(value.StoragePath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return nil, fmt.Errorf("open attachment root: %w", err)
	}
	defer root.Close()
	if err := rejectSymlinks(root, rel); err != nil {
		return nil, err
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("open attachment content: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect attachment content: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrUnsafePath
	}
	if info.Size() != value.SizeBytes {
		return nil, ErrContentChanged
	}

	var content bytes.Buffer
	hash := sha256.New()
	if _, err := copyLimitedContext(ctx, io.MultiWriter(&content, hash), file, r.maxSize); err != nil {
		return nil, err
	}
	if int64(content.Len()) != value.SizeBytes || subtle.ConstantTimeCompare(hash.Sum(nil), expected) != 1 {
		return nil, ErrContentChanged
	}
	return content.Bytes(), nil
}

func (r *ContentReader) relativePath(storagePath string) (string, error) {
	if strings.TrimSpace(storagePath) == "" {
		return "", ErrUnsafePath
	}
	path := filepath.Clean(storagePath)
	if filepath.IsAbs(path) {
		var err error
		path, err = filepath.Rel(r.root, path)
		if err != nil {
			return "", ErrUnsafePath
		}
	}
	if path == "." || path == ".." || filepath.IsAbs(path) || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", ErrUnsafePath
	}
	return path, nil
}

func rejectSymlinks(root *os.Root, path string) error {
	current := ""
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect attachment path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		if index < len(parts)-1 && !info.IsDir() {
			return ErrUnsafePath
		}
	}
	return nil
}
