package skillhub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"
)

type memoryProfessionalStore struct {
	mu      sync.RWMutex
	objects map[string][]byte
	digests map[string]string
}

func newMemoryProfessionalStore() *memoryProfessionalStore {
	return &memoryProfessionalStore{
		objects: map[string][]byte{},
		digests: map[string]string{},
	}
}

func (*memoryProfessionalStore) CheckConnectivity(context.Context) error { return nil }

func (*memoryProfessionalStore) Reserve(
	tenantID uint64,
	skillID string,
	revisionID string,
) (string, error) {
	return fmt.Sprintf(
		"memory://professional/tenant/%d/skill/%s/revision/%s/package.zip",
		tenantID,
		skillID,
		revisionID,
	), nil
}

func (s *memoryProfessionalStore) CommitAndVerify(
	_ context.Context,
	data []byte,
	path string,
	_ string,
	digest string,
) error {
	actual := fmt.Sprintf("%x", sha256.Sum256(data))
	if actual != digest {
		return fmt.Errorf("digest mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[path] = bytes.Clone(data)
	s.digests[path] = digest
	return nil
}

func (s *memoryProfessionalStore) Verify(
	_ context.Context,
	path string,
	size int64,
	digest string,
) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.objects[path]
	if !ok {
		return fmt.Errorf("object not found")
	}
	if int64(len(data)) != size || s.digests[path] != digest {
		return fmt.Errorf("object verification failed")
	}
	return nil
}

func (s *memoryProfessionalStore) Open(
	_ context.Context,
	path string,
) (io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.objects[path]
	if !ok {
		return nil, fmt.Errorf("object not found")
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), nil
}

func (s *memoryProfessionalStore) Delete(_ context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, path)
	delete(s.digests, path)
	return nil
}

func (s *memoryProfessionalStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.objects)
}
