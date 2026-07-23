package repository

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var (
	ErrUnknownRepository   = errors.New("repository: unknown repository")
	ErrFingerprintMismatch = errors.New("repository: fingerprint mismatch")
	ErrRootChanged         = errors.New("repository: registered root changed")
	ErrOutsideRoot         = errors.New("repository: path escapes registered root")
	ErrSymlink             = errors.New("repository: symlink path is not an approved root")
	ErrNested              = errors.New("repository: nested repository registrations are not allowed")
	ErrWorktreeUnavailable = errors.New("repository: worktree root is not configured")
	ErrInvalidPath         = errors.New("repository: invalid local path")
)

// Descriptor is the only repository identity that may cross a managed
// transport boundary. It intentionally has no local path field.
type Descriptor struct {
	ID          string `json:"repository_id"`
	Fingerprint string `json:"fingerprint"`
}

type MessageRef struct {
	RepositoryID string `json:"repository_id"`
	Fingerprint  string `json:"fingerprint"`
}

type Registration struct {
	RootPath     string
	WorktreeRoot string
	RemoteURL    string
}

type registryEntry struct {
	descriptor        Descriptor
	root              string
	canonicalRoot     string
	rootIdentity      fileIdentity
	worktreeRoot      string
	canonicalWorktree string
}

// Handle is an in-process capability for a registered repository. Its path
// methods are intentionally not serializable; callers must use MessageRef for
// transport and presentation.
type Handle struct{ entry registryEntry }

type Worktree struct {
	handle   Handle
	path     string
	identity fileIdentity
}

type Registry struct {
	mu      sync.RWMutex
	entries map[string]registryEntry
}

func NewRegistry() *Registry { return &Registry{entries: make(map[string]registryEntry)} }

func (r *Registry) RegisterPath(root string) (Descriptor, error) {
	return r.Register(Registration{RootPath: root})
}

func (r *Registry) Register(input Registration) (Descriptor, error) {
	if r == nil {
		return Descriptor{}, errors.New("repository: nil registry")
	}
	root, canonicalRoot, info, identity, err := approvedRoot(input.RootPath)
	if err != nil {
		return Descriptor{}, err
	}
	canonicalWorktree, err := approvedAnchor(input.WorktreeRoot)
	if err != nil {
		return Descriptor{}, err
	}
	if canonicalWorktree != "" && pathsOverlap(canonicalRoot, canonicalWorktree) {
		return Descriptor{}, fmt.Errorf("%w: worktree root overlaps repository root", ErrNested)
	}
	id := makeOpaqueID()
	entry := registryEntry{
		descriptor:        Descriptor{ID: id, Fingerprint: fingerprintFor(canonicalRoot, info, identity)},
		root:              root,
		canonicalRoot:     canonicalRoot,
		rootIdentity:      identity,
		worktreeRoot:      cleanAbsolute(input.WorktreeRoot),
		canonicalWorktree: canonicalWorktree,
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.entries {
		if pathsOverlap(existing.canonicalRoot, entry.canonicalRoot) ||
			(entry.canonicalWorktree != "" && pathsOverlap(existing.canonicalRoot, entry.canonicalWorktree)) ||
			(existing.canonicalWorktree != "" && pathsOverlap(existing.canonicalWorktree, entry.canonicalRoot)) ||
			(entry.canonicalWorktree != "" && existing.canonicalWorktree != "" && pathsOverlap(existing.canonicalWorktree, entry.canonicalWorktree)) {
			return Descriptor{}, ErrNested
		}
	}
	r.entries[id] = entry
	return entry.descriptor, nil
}

func (r *Registry) Open(id, fingerprint string) (Handle, error) {
	if r == nil {
		return Handle{}, ErrUnknownRepository
	}
	r.mu.RLock()
	entry, ok := r.entries[id]
	r.mu.RUnlock()
	if !ok {
		return Handle{}, fmt.Errorf("%w: %s", ErrUnknownRepository, id)
	}
	if entry.descriptor.Fingerprint != fingerprint {
		return Handle{}, ErrFingerprintMismatch
	}
	handle := Handle{entry: entry}
	if err := handle.Validate(); err != nil {
		return Handle{}, err
	}
	return handle, nil
}

func (r *Registry) Lookup(id string) (Descriptor, error) {
	if r == nil {
		return Descriptor{}, ErrUnknownRepository
	}
	r.mu.RLock()
	entry, ok := r.entries[id]
	r.mu.RUnlock()
	if !ok {
		return Descriptor{}, fmt.Errorf("%w: %s", ErrUnknownRepository, id)
	}
	return entry.descriptor, nil
}

func (r *Registry) List() []Descriptor {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	values := make([]Descriptor, 0, len(r.entries))
	for _, entry := range r.entries {
		values = append(values, entry.descriptor)
	}
	r.mu.RUnlock()
	for i := 0; i < len(values); i++ {
		for j := i + 1; j < len(values); j++ {
			if values[j].ID < values[i].ID {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
	return values
}

func (h Handle) Descriptor() Descriptor { return h.entry.descriptor }
func (h Handle) MessageRef() MessageRef {
	return MessageRef{RepositoryID: h.entry.descriptor.ID, Fingerprint: h.entry.descriptor.Fingerprint}
}
func (h Handle) Path() string         { return h.entry.canonicalRoot }
func (h Handle) WorktreeRoot() string { return h.entry.canonicalWorktree }

func (h Handle) Validate() error {
	if h.entry.canonicalRoot == "" {
		return ErrUnknownRepository
	}
	info, err := os.Stat(h.entry.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrRootChanged
		}
		return fmt.Errorf("validate repository root: %w", err)
	}
	identity, err := identityFor(info)
	if err != nil || !sameIdentity(h.entry.rootIdentity, identity) {
		return ErrRootChanged
	}
	canonical, err := os.EvalSymlinks(h.entry.root)
	if err != nil || !samePath(canonical, h.entry.canonicalRoot) {
		return ErrRootChanged
	}
	if !info.IsDir() {
		return ErrRootChanged
	}
	return nil
}

func (h Handle) Contain(path string) (string, error) {
	if err := h.Validate(); err != nil {
		return "", err
	}
	abs, err := validateLocalPath(path)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalCandidate(abs)
	if err != nil {
		return "", err
	}
	if isWithin(h.entry.canonicalRoot, canonical) || (h.entry.canonicalWorktree != "" && isWithin(h.entry.canonicalWorktree, canonical)) {
		return canonical, nil
	}
	return "", ErrOutsideRoot
}

func (h Handle) OpenWorktree(path string) (Worktree, error) {
	if h.entry.canonicalWorktree == "" {
		return Worktree{}, ErrWorktreeUnavailable
	}
	contained, err := h.Contain(path)
	if err != nil {
		return Worktree{}, err
	}
	if !isWithin(h.entry.canonicalWorktree, contained) || samePath(h.entry.canonicalRoot, contained) {
		return Worktree{}, ErrOutsideRoot
	}
	info, err := os.Stat(contained)
	if err != nil || !info.IsDir() {
		return Worktree{}, ErrOutsideRoot
	}
	identity, err := identityFor(info)
	if err != nil {
		return Worktree{}, err
	}
	return Worktree{handle: h, path: contained, identity: identity}, nil
}

func (w Worktree) Path() string { return w.path }

func (w Worktree) Validate() error {
	if err := w.handle.Validate(); err != nil {
		return err
	}
	info, err := os.Stat(w.path)
	if err != nil || !info.IsDir() {
		return ErrOutsideRoot
	}
	identity, err := identityFor(info)
	if err != nil || !sameIdentity(w.identity, identity) {
		return ErrRootChanged
	}
	canonical, err := os.EvalSymlinks(w.path)
	if err != nil || !samePath(canonical, w.path) || !isWithin(w.handle.entry.canonicalWorktree, canonical) {
		return ErrOutsideRoot
	}
	return nil
}

func approvedRoot(path string) (string, string, os.FileInfo, fileIdentity, error) {
	clean, err := validateLocalPath(path)
	if err != nil {
		return "", "", nil, fileIdentity{}, err
	}
	lstat, err := os.Lstat(clean)
	if err != nil {
		return "", "", nil, fileIdentity{}, fmt.Errorf("register repository root: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		return "", "", nil, fileIdentity{}, ErrSymlink
	}
	canonical, err := os.EvalSymlinks(clean)
	if err != nil {
		return "", "", nil, fileIdentity{}, fmt.Errorf("resolve repository root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", "", nil, fileIdentity{}, errors.New("repository: root must be a directory")
	}
	identity, err := identityFor(info)
	if err != nil {
		return "", "", nil, fileIdentity{}, err
	}
	return clean, canonical, info, identity, nil
}

func approvedAnchor(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	clean, err := validateLocalPath(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(clean); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", ErrSymlink
		}
		if !info.IsDir() {
			return "", errors.New("repository: worktree root must be a directory")
		}
		return os.EvalSymlinks(clean)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(clean)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("prepare worktree root: %w", err)
	}
	canonicalParent, err := os.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root parent: %w", err)
	}
	return filepath.Join(canonicalParent, filepath.Base(clean)), nil
}

func canonicalCandidate(path string) (string, error) {
	if _, err := os.Lstat(path); err == nil {
		canonical, err := os.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("resolve repository path: %w", err)
		}
		return canonical, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect repository path: %w", err)
	}
	missing := make([]string, 0, 4)
	current := path
	for {
		if _, err := os.Lstat(current); err == nil {
			canonical, err := os.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				canonical = filepath.Join(canonical, missing[i])
			}
			return canonical, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", ErrInvalidPath
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func validateLocalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || hasDotDot(path) {
		return "", ErrInvalidPath
	}
	return filepath.Clean(path), nil
}

func cleanAbsolute(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func hasDotDot(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

func pathsOverlap(left, right string) bool {
	return isWithin(left, right) || isWithin(right, left)
}

func isWithin(root, candidate string) bool {
	if root == "" || candidate == "" {
		return false
	}
	rel, err := filepath.Rel(root, candidate)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return true
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		return false
	}
	rootParts := splitPath(root)
	candidateParts := splitPath(candidate)
	if len(candidateParts) < len(rootParts) {
		return false
	}
	for i := range rootParts {
		if !strings.EqualFold(rootParts[i], candidateParts[i]) {
			return false
		}
	}
	return true
}

func samePath(left, right string) bool {
	if left == right {
		return true
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return false
}

func splitPath(path string) []string {
	return strings.FieldsFunc(filepath.Clean(path), func(r rune) bool { return r == '/' || r == '\\' })
}

func makeOpaqueID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "repo-" + hex.EncodeToString([]byte(fmt.Sprintf("%d", os.Getpid())))
	}
	return "repo-" + hex.EncodeToString(buffer)
}
