package attachment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestContentReaderReadsVerifiedFileInsideRoot(t *testing.T) {
	root := t.TempDir()
	content := []byte("verified dashboard image")
	path := filepath.Join(root, "image.png")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewContentReader(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	got, err := reader.Read(context.Background(), workmodel.Attachment{
		StoragePath: path,
		SizeBytes:   int64(len(content)),
		SHA256:      hex.EncodeToString(digest[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("content = %q", got)
	}
}

func TestContentReaderRejectsUnsafePathsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "link.png")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	reader, err := NewContentReader(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"outside":   outside,
		"traversal": filepath.Join(root, "..", filepath.Base(outside)),
		"symlink":   symlink,
	} {
		_, err := reader.Read(context.Background(), workmodel.Attachment{StoragePath: path, SizeBytes: 7, SHA256: validDigest("outside")})
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("%s error = %v, want ErrUnsafePath", name, err)
		}
	}
}

func TestContentReaderRejectsSizeChecksumAndNonRegularFileChanges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "image.png")
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewContentReader(root, 32)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		attachment workmodel.Attachment
		want       error
	}{
		{"metadata exceeds limit", workmodel.Attachment{StoragePath: path, SizeBytes: 33, SHA256: validDigest("changed")}, ErrTooLarge},
		{"size mismatch", workmodel.Attachment{StoragePath: path, SizeBytes: 6, SHA256: validDigest("changed")}, ErrContentChanged},
		{"checksum mismatch", workmodel.Attachment{StoragePath: path, SizeBytes: 7, SHA256: validDigest("original")}, ErrContentChanged},
		{"invalid checksum", workmodel.Attachment{StoragePath: path, SizeBytes: 7, SHA256: "not-a-digest"}, ErrContentChanged},
		{"directory", workmodel.Attachment{StoragePath: root, SizeBytes: 0, SHA256: validDigest("")}, ErrUnsafePath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := reader.Read(context.Background(), tt.attachment)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestContentReaderHonorsCanceledContext(t *testing.T) {
	reader, err := NewContentReader(t.TempDir(), 32)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = reader.Read(ctx, workmodel.Attachment{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func validDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
