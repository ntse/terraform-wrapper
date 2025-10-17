package lock_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"

	"terraform-wrapper/internal/lock"
)

func TestAcquireReleaseLock(t *testing.T) {
	t.Parallel()

	s3stub := newMemoryS3()
	l := &lock.OrchestrationLock{
		Bucket:       "test-bucket",
		Env:          "dev",
		Owner:        "unit-test",
		Command:      "plan-all",
		Client:       s3stub,
		TTL:          time.Minute,
		PollInterval: 50 * time.Millisecond,
	}

	ctx := context.Background()
	require.NoError(t, l.Acquire(ctx, false, false))
	require.True(t, s3stub.exists(lockKey(l.Env)))

	require.NoError(t, l.Release(ctx))
	require.False(t, s3stub.exists(lockKey(l.Env)))
}

func TestAcquireWhileLockedReturnsError(t *testing.T) {
	t.Parallel()

	s3stub := newMemoryS3()
	key := lockKey("dev")
	s3stub.putExisting(key, map[string]string{
		"owner":     "worker-a",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"command":   "apply-all",
	})

	l := &lock.OrchestrationLock{
		Bucket:       "test",
		Env:          "dev",
		Owner:        "worker-b",
		Command:      "plan-all",
		Client:       s3stub,
		TTL:          time.Minute,
		PollInterval: 10 * time.Millisecond,
	}

	ctx := context.Background()
	err := l.Acquire(ctx, false, false)
	require.Error(t, err)

	var lockedErr *lock.LockedError
	require.ErrorAs(t, err, &lockedErr)
	require.Equal(t, "worker-a", lockedErr.Owner)
	require.Equal(t, "apply-all", lockedErr.Command)
	require.Equal(t, "dev", lockedErr.Env)
	require.Equal(t, lock.LockedExitCode, lockedErr.ExitCode())
}

func TestAcquireWaitsUntilReleased(t *testing.T) {
	t.Parallel()

	s3stub := newMemoryS3()
	key := lockKey("dev")
	s3stub.putExisting(key, map[string]string{
		"owner":     "first",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})

	l := &lock.OrchestrationLock{
		Bucket:       "test",
		Env:          "dev",
		Client:       s3stub,
		TTL:          time.Minute,
		PollInterval: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan error)
	go func() {
		done <- l.Acquire(ctx, true, false)
	}()

	// release after short delay
	time.AfterFunc(150*time.Millisecond, func() {
		s3stub.delete(key)
	})

	err := <-done
	require.NoError(t, err)
	require.True(t, s3stub.exists(key))
}

func TestAcquireForceStaleLock(t *testing.T) {
	t.Parallel()

	s3stub := newMemoryS3()
	key := lockKey("dev")
	stale := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	s3stub.putExisting(key, map[string]string{
		"owner":     "stale-worker",
		"timestamp": stale,
	})

	l := &lock.OrchestrationLock{
		Bucket:       "test",
		Env:          "dev",
		Client:       s3stub,
		TTL:          30 * time.Minute,
		PollInterval: 10 * time.Millisecond,
	}

	require.NoError(t, l.Acquire(context.Background(), false, true))
	require.True(t, s3stub.exists(key))
	meta := s3stub.metadata(key)
	require.Equal(t, l.Owner, meta["owner"])
}

// memoryS3 implements a minimal in-memory S3API for testing.
type memoryS3 struct {
	mu      sync.Mutex
	objects map[string]*s3Object
}

type s3Object struct {
	body     []byte
	metadata map[string]string
}

func newMemoryS3() *memoryS3 {
	return &memoryS3{
		objects: make(map[string]*s3Object),
	}
}

func (m *memoryS3) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := aws.ToString(params.Key)
	if _, exists := m.objects[key]; exists {
		return nil, fmt.Errorf("PreconditionFailed: object exists")
	}

	body, _ := io.ReadAll(params.Body)
	meta := map[string]string{}
	if params.Metadata != nil {
		meta = make(map[string]string, len(params.Metadata))
		for k, v := range params.Metadata {
			meta[strings.ToLower(k)] = v
		}
	}

	m.objects[key] = &s3Object{
		body:     body,
		metadata: meta,
	}

	return &s3.PutObjectOutput{}, nil
}

func (m *memoryS3) HeadObject(_ context.Context, params *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	obj, ok := m.objects[aws.ToString(params.Key)]
	if !ok {
		return nil, fmt.Errorf("NotFound")
	}

	return &s3.HeadObjectOutput{
		Metadata: obj.metadata,
	}, nil
}

func (m *memoryS3) DeleteObject(_ context.Context, params *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.objects, aws.ToString(params.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (m *memoryS3) exists(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

func (m *memoryS3) metadata(key string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if obj, ok := m.objects[key]; ok {
		cp := make(map[string]string, len(obj.metadata))
		for k, v := range obj.metadata {
			cp[k] = v
		}
		return cp
	}
	return nil
}

func (m *memoryS3) delete(key string) {
	m.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String("test"),
		Key:    aws.String(key),
	})
}

func (m *memoryS3) putExisting(key string, metadata map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta := make(map[string]string, len(metadata))
	for k, v := range metadata {
		meta[strings.ToLower(k)] = v
	}
	m.objects[key] = &s3Object{
		body:     []byte("{}"),
		metadata: meta,
	}
}

func lockKey(env string) string {
	return fmt.Sprintf("locks/%s/superplan-lock.json", env)
}
