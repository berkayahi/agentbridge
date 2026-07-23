package attachment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

func TestServiceAcceptsDetectedJPEGPNGAndWebPAndIgnoresRemoteFilename(t *testing.T) {
	tests := []struct {
		name, mime string
		data       []byte
		ext        string
	}{
		{"jpeg", "image/jpeg", append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0}, 32)...), ".jpg"},
		{"png", "image/png", append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...), ".png"},
		{"webp", "image/webp", append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0}, 32)...), ".webp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &memoryStore{}
			svc := newTestService(t, store, &fakeSource{data: tt.data}, &fakeLocator{active: []string{"task-1"}})
			got, err := svc.Save(context.Background(), IncomingFile{FileID: "remote", RemoteFilename: "../../payload.exe", DeclaredMediaType: tt.mime, ChatID: 100, ReceivedAt: time.Unix(1000, 0)})
			if err != nil {
				t.Fatal(err)
			}
			if got.MediaType != tt.mime || filepath.Ext(got.StoragePath) != tt.ext || got.Name != "id-1"+tt.ext || strings.Contains(got.StoragePath, "payload") {
				t.Fatalf("attachment=%#v", got)
			}
			if _, err := os.Stat(got.StoragePath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServiceRejectsOversizeMismatchExecutableAndDuplicateAndCleansUp(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	tests := []struct {
		name     string
		source   *fakeSource
		incoming IncomingFile
		want     error
	}{
		{"oversize", &fakeSource{data: bytes.Repeat([]byte("x"), 129)}, IncomingFile{FileID: "1", DeclaredMediaType: "image/png", ChatID: 100}, ErrTooLarge},
		{"mime mismatch", &fakeSource{data: png}, IncomingFile{FileID: "1", DeclaredMediaType: "image/jpeg", ChatID: 100}, ErrMediaType},
		{"executable", &fakeSource{data: []byte("#!/bin/sh\necho bad")}, IncomingFile{FileID: "1", DeclaredMediaType: "image/png", ChatID: 100}, ErrMediaType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &memoryStore{}
			svc := newTestService(t, store, tt.source, &fakeLocator{active: []string{"task-1"}})
			_, err := svc.Save(context.Background(), tt.incoming)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error=%v", err)
			}
			assertDirectoryEmpty(t, svc.root)
		})
	}
	store := &memoryStore{}
	svc := newTestService(t, store, &fakeSource{data: png}, &fakeLocator{active: []string{"task-1"}})
	in := IncomingFile{FileID: "1", DeclaredMediaType: "image/png", ChatID: 100}
	if _, err := svc.Save(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(context.Background(), in); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("error=%v", err)
	}
}

func TestServiceHonorsCanceledDownload(t *testing.T) {
	svc := newTestService(t, &memoryStore{}, &fakeSource{data: []byte("ignored")}, &fakeLocator{active: []string{"task-1"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.Save(ctx, IncomingFile{FileID: "1", ChatID: 100})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	assertDirectoryEmpty(t, svc.root)
}

func TestAssociationCaptionReplyAlbumDraftAndSingleActiveTask(t *testing.T) {
	now := time.Unix(1000, 0)
	locator := &fakeLocator{captionTask: "caption-task", replyTask: "reply-task", active: []string{"active-task"}}
	svc := newTestService(t, &memoryStore{}, &fakeSource{data: validPNG(), vary: true}, locator)
	svc.now = func() time.Time { return now }
	svc.draftWindow = time.Minute
	tests := []struct {
		name string
		in   IncomingFile
		want string
	}{
		{"caption", IncomingFile{FileID: "1", ChatID: 1, Caption: "/codex inspect", MediaGroupID: "album", ReceivedAt: now}, "caption-task"},
		{"album", IncomingFile{FileID: "2", ChatID: 1, MediaGroupID: "album", ReceivedAt: now}, "caption-task"},
		{"draft", IncomingFile{FileID: "3", ChatID: 1, ReceivedAt: now.Add(30 * time.Second)}, "caption-task"},
		{"reply", IncomingFile{FileID: "4", ChatID: 2, ReplyToMessageID: 77, ReceivedAt: now}, "reply-task"},
		{"single active", IncomingFile{FileID: "5", ChatID: 3, ReceivedAt: now}, "active-task"},
	}
	for _, tt := range tests {
		got, err := svc.Save(context.Background(), tt.in)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got.TaskID != tt.want {
			t.Fatalf("%s task=%q", tt.name, got.TaskID)
		}
	}
}

func TestAssociationRejectsAmbiguousAndOrphanWithHelpfulResult(t *testing.T) {
	for _, tt := range []struct {
		name   string
		active []string
		want   error
	}{{"ambiguous", []string{"a", "b"}, ErrAmbiguous}, {"orphan", nil, ErrOrphan}} {
		svc := newTestService(t, &memoryStore{}, &fakeSource{data: validPNG()}, &fakeLocator{active: tt.active})
		_, err := svc.Save(context.Background(), IncomingFile{FileID: "1", ChatID: 1})
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), "reply") {
			t.Fatalf("%s error=%v", tt.name, err)
		}
	}
}

func newTestService(t *testing.T, store MetadataStore, source FileSource, locator TaskLocator) *Service {
	t.Helper()
	n := 0
	svc, err := NewService(t.TempDir(), 128, time.Minute, store, source, locator, func() (string, error) { n++; return "id-" + string(rune('0'+n)), nil }, func() time.Time { return time.Unix(1000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return svc
}
func validPNG() []byte { return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...) }
func assertDirectoryEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries=%v", entries)
	}
}

type fakeSource struct {
	data  []byte
	err   error
	vary  bool
	calls byte
}

func (f *fakeSource) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	data := append([]byte(nil), f.data...)
	if f.vary {
		f.calls++
		data = append(data, f.calls)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type fakeLocator struct {
	captionTask, replyTask string
	active                 []string
}

func (f *fakeLocator) TaskForCaption(context.Context, int64, string) (string, error) {
	return f.captionTask, nil
}
func (f *fakeLocator) TaskForStatusMessage(context.Context, int64, int64) (string, error) {
	return f.replyTask, nil
}
func (f *fakeLocator) ActiveTaskIDs(context.Context, int64) ([]string, error) {
	return append([]string(nil), f.active...), nil
}

type memoryStore struct{ attachments []workmodel.Attachment }

func (m *memoryStore) SaveAttachment(ctx context.Context, a workmodel.Attachment) error {
	m.attachments = append(m.attachments, a)
	return nil
}
func (m *memoryStore) Attachments(ctx context.Context, taskID string) ([]workmodel.Attachment, error) {
	var out []workmodel.Attachment
	for _, a := range m.attachments {
		if a.TaskID == taskID {
			out = append(out, a)
		}
	}
	return out, nil
}
