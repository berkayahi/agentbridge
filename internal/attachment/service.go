// Package attachment safely stores and associates Telegram screenshots.
package attachment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

var (
	ErrTooLarge  = errors.New("attachment: file is too large")
	ErrMediaType = errors.New("attachment: only JPEG, PNG, and WebP images are accepted")
	ErrDuplicate = errors.New("attachment: duplicate image")
	ErrAmbiguous = errors.New("attachment: multiple tasks match; reply to a task status message")
	ErrOrphan    = errors.New("attachment: no task matches; send a command or reply to a task status message")
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

type IncomingFile struct {
	FileID            string
	RemoteFilename    string
	DeclaredMediaType string
	Caption           string
	ChatID            int64
	ReplyToMessageID  int64
	MediaGroupID      string
	ReceivedAt        time.Time
}

type FileSource interface {
	Open(context.Context, string) (io.ReadCloser, error)
}

type MetadataStore interface {
	SaveAttachment(context.Context, workmodel.Attachment) error
	Attachments(context.Context, string) ([]workmodel.Attachment, error)
}

type TaskLocator interface {
	TaskForCaption(context.Context, int64, string) (string, error)
	TaskForStatusMessage(context.Context, int64, int64) (string, error)
	ActiveTaskIDs(context.Context, int64) ([]string, error)
}
type explicitTaskLocator interface {
	TaskForID(context.Context, int64, string) (string, error)
}

type draft struct {
	taskID  string
	expires time.Time
}

type Service struct {
	root        string
	maxSize     int64
	draftWindow time.Duration
	store       MetadataStore
	source      FileSource
	locator     TaskLocator
	newID       func() (string, error)
	now         func() time.Time
	mu          sync.Mutex
	drafts      map[int64]draft
	albums      map[string]draft
}

func NewService(root string, maxSize int64, draftWindow time.Duration, store MetadataStore, source FileSource, locator TaskLocator, newID func() (string, error), now func() time.Time) (*Service, error) {
	if strings.TrimSpace(root) == "" || maxSize < 1 || store == nil || source == nil || locator == nil {
		return nil, errors.New("attachment: invalid service configuration")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create attachment directory: %w", err)
	}
	if newID == nil {
		newID = randomID
	}
	if now == nil {
		now = time.Now
	}
	if draftWindow < 0 {
		draftWindow = 0
	}
	return &Service{root: root, maxSize: maxSize, draftWindow: draftWindow, store: store, source: source, locator: locator, newID: newID, now: now, drafts: make(map[int64]draft), albums: make(map[string]draft)}, nil
}

func (s *Service) Save(ctx context.Context, in IncomingFile) (workmodel.Attachment, error) {
	if err := ctx.Err(); err != nil {
		return workmodel.Attachment{}, err
	}
	if in.FileID == "" || in.ChatID == 0 {
		return workmodel.Attachment{}, errors.New("attachment: file and chat IDs are required")
	}
	taskID, err := s.associate(ctx, in)
	if err != nil {
		return workmodel.Attachment{}, err
	}
	return s.saveForTask(ctx, taskID, in)
}

// SaveForTask associates a file with the task the trusted command dispatcher
// just created, while still verifying the task belongs to the same chat.
func (s *Service) SaveForTask(ctx context.Context, taskID string, in IncomingFile) (workmodel.Attachment, error) {
	locator, ok := s.locator.(explicitTaskLocator)
	if !ok {
		return workmodel.Attachment{}, ErrTaskReference
	}
	verified, err := locator.TaskForID(ctx, in.ChatID, taskID)
	if err != nil {
		return workmodel.Attachment{}, err
	}
	if verified != taskID {
		return workmodel.Attachment{}, ErrTaskReference
	}
	at := in.ReceivedAt
	if at.IsZero() {
		at = s.now()
	}
	s.remember(in, taskID, at)
	return s.saveForTask(ctx, taskID, in)
}

func (s *Service) saveForTask(ctx context.Context, taskID string, in IncomingFile) (workmodel.Attachment, error) {
	reader, err := s.source.Open(ctx, in.FileID)
	if err != nil {
		return workmodel.Attachment{}, fmt.Errorf("open Telegram attachment: %w", err)
	}
	defer reader.Close()
	temp, err := os.CreateTemp(s.root, ".incoming-*")
	if err != nil {
		return workmodel.Attachment{}, fmt.Errorf("create attachment temporary file: %w", err)
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	hash := sha256.New()
	size, err := copyLimitedContext(ctx, io.MultiWriter(temp, hash), reader, s.maxSize)
	if err != nil {
		return workmodel.Attachment{}, err
	}
	if err := temp.Sync(); err != nil {
		return workmodel.Attachment{}, fmt.Errorf("sync attachment: %w", err)
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return workmodel.Attachment{}, fmt.Errorf("inspect attachment: %w", err)
	}
	header := make([]byte, 512)
	n, readErr := io.ReadFull(temp, header)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return workmodel.Attachment{}, fmt.Errorf("inspect attachment: %w", readErr)
	}
	mediaType, extension, err := acceptedMediaType(header[:n], in.DeclaredMediaType)
	if err != nil {
		return workmodel.Attachment{}, err
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	existing, err := s.store.Attachments(ctx, taskID)
	if err != nil {
		return workmodel.Attachment{}, fmt.Errorf("list task attachments: %w", err)
	}
	for _, value := range existing {
		if value.SHA256 == checksum {
			return workmodel.Attachment{}, ErrDuplicate
		}
	}
	id, err := s.newID()
	if err != nil {
		return workmodel.Attachment{}, fmt.Errorf("generate attachment ID: %w", err)
	}
	if !safeID.MatchString(id) {
		return workmodel.Attachment{}, errors.New("attachment: generated unsafe ID")
	}
	name := id + extension
	finalPath := filepath.Join(s.root, name)
	if err := temp.Close(); err != nil {
		return workmodel.Attachment{}, fmt.Errorf("close attachment: %w", err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return workmodel.Attachment{}, fmt.Errorf("publish attachment: %w", err)
	}
	tempPath = finalPath
	if err := syncDirectory(s.root); err != nil {
		return workmodel.Attachment{}, err
	}
	createdAt := s.now()
	if !in.ReceivedAt.IsZero() {
		createdAt = in.ReceivedAt
	}
	value := workmodel.Attachment{ID: id, TaskID: taskID, Kind: "image", Name: name, MediaType: mediaType, StoragePath: finalPath, SizeBytes: size, SHA256: checksum, CreatedAt: createdAt}
	if err := s.store.SaveAttachment(ctx, value); err != nil {
		return workmodel.Attachment{}, fmt.Errorf("save attachment metadata: %w", err)
	}
	keep = true
	return value, nil
}

func (s *Service) associate(ctx context.Context, in IncomingFile) (string, error) {
	at := in.ReceivedAt
	if at.IsZero() {
		at = s.now()
	}
	if strings.TrimSpace(in.Caption) != "" {
		taskID, err := s.locator.TaskForCaption(ctx, in.ChatID, in.Caption)
		if err != nil {
			return "", fmt.Errorf("resolve attachment caption: %w", err)
		}
		if taskID != "" {
			s.remember(in, taskID, at)
			return taskID, nil
		}
	}
	if in.ReplyToMessageID != 0 {
		taskID, err := s.locator.TaskForStatusMessage(ctx, in.ChatID, in.ReplyToMessageID)
		if err != nil {
			return "", fmt.Errorf("resolve status reply: %w", err)
		}
		if taskID != "" {
			s.remember(in, taskID, at)
			return taskID, nil
		}
	}
	if taskID := s.remembered(in, at); taskID != "" {
		return taskID, nil
	}
	active, err := s.locator.ActiveTaskIDs(ctx, in.ChatID)
	if err != nil {
		return "", fmt.Errorf("list active tasks: %w", err)
	}
	if len(active) > 1 {
		return "", ErrAmbiguous
	}
	if len(active) == 0 {
		return "", ErrOrphan
	}
	s.remember(in, active[0], at)
	return active[0], nil
}

func (s *Service) remember(in IncomingFile, taskID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := draft{taskID: taskID, expires: at.Add(s.draftWindow)}
	s.drafts[in.ChatID] = value
	if in.MediaGroupID != "" {
		s.albums[albumKey(in.ChatID, in.MediaGroupID)] = value
	}
}
func (s *Service) remembered(in IncomingFile, at time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.MediaGroupID != "" {
		key := albumKey(in.ChatID, in.MediaGroupID)
		if value, ok := s.albums[key]; ok {
			if !at.After(value.expires) {
				return value.taskID
			}
			delete(s.albums, key)
		}
	}
	if value, ok := s.drafts[in.ChatID]; ok {
		if !at.After(value.expires) {
			return value.taskID
		}
		delete(s.drafts, in.ChatID)
	}
	return ""
}
func albumKey(chatID int64, group string) string { return fmt.Sprintf("%d:%s", chatID, group) }

func copyLimitedContext(ctx context.Context, dst io.Writer, src io.Reader, max int64) (int64, error) {
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			if total+int64(n) > max {
				return total, ErrTooLarge
			}
			written, writeErr := dst.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil {
				return total, fmt.Errorf("write attachment: %w", writeErr)
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, fmt.Errorf("read attachment: %w", readErr)
		}
	}
}

func acceptedMediaType(header []byte, declared string) (string, string, error) {
	detected := strings.Split(http.DetectContentType(header), ";")[0]
	extensions := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}
	extension, ok := extensions[detected]
	if !ok {
		return "", "", ErrMediaType
	}
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if declared == "image/jpg" {
		declared = "image/jpeg"
	}
	if declared != "" && declared != detected {
		return "", "", ErrMediaType
	}
	return detected, extension, nil
}
func randomID() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open attachment directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync attachment directory: %w", err)
	}
	return nil
}
