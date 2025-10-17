package lock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const (
	// LockedExitCode mirrors the desired CLI exit status when an orchestration lock is held.
	LockedExitCode = 65
	defaultTTL     = time.Hour
	defaultPoll    = 15 * time.Second
)

// S3API captures the subset of S3 operations required by the orchestration lock.
type S3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// LockedError conveys that an environment is currently locked by another actor.
type LockedError struct {
	Env       string
	Owner     string
	Command   string
	Timestamp time.Time
}

func (e *LockedError) Error() string {
	if e == nil {
		return "environment locked"
	}
	ts := e.Timestamp.Format(time.RFC3339)
	return fmt.Sprintf("environment %q is locked by %s since %s", e.Env, e.Owner, ts)
}

func (e *LockedError) ExitCode() int {
	return LockedExitCode
}

// OrchestrationLock represents a global environment-level lock stored in S3.
type OrchestrationLock struct {
	Bucket       string
	Env          string
	Owner        string
	Command      string
	TTL          time.Duration
	PollInterval time.Duration
	Client       S3API

	mu     sync.Mutex
	locked bool
}

// key returns the S3 key for the orchestration lock.
func (l *OrchestrationLock) key() string {
	return fmt.Sprintf("locks/%s/superplan-lock.json", l.Env)
}

// Acquire attempts to acquire the orchestration lock, optionally waiting or forcing
// stale locks based on the provided flags.
func (l *OrchestrationLock) Acquire(ctx context.Context, wait bool, force bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.Client == nil {
		return fmt.Errorf("lock client must not be nil")
	}
	if l.Bucket == "" {
		return fmt.Errorf("lock bucket must not be empty")
	}
	if l.Env == "" {
		return fmt.Errorf("lock environment must not be empty")
	}

	if l.Owner == "" {
		l.Owner = defaultOwner()
	}
	if l.TTL <= 0 {
		l.TTL = defaultTTL
	}
	if l.PollInterval <= 0 {
		l.PollInterval = defaultPoll
	}

	lockData := map[string]string{
		"owner":     l.Owner,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"env":       l.Env,
	}
	if l.Command != "" {
		lockData["command"] = l.Command
	}

	payload, _ := json.Marshal(lockData)
	metadata := map[string]string{
		"owner":     l.Owner,
		"timestamp": lockData["timestamp"],
	}
	if l.Command != "" {
		metadata["command"] = l.Command
	}

	for {
		_, err := l.Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(l.Bucket),
			Key:         aws.String(l.key()),
			Body:        strings.NewReader(string(payload)),
			ContentType: aws.String("application/json"),
			Metadata:    metadata,
		}, func(o *s3.Options) {
			o.APIOptions = append(o.APIOptions, ifNoneMatchOption("*"))
		})
		if err == nil {
			fmt.Printf("🔒 Acquired orchestration lock for %s\n", l.Env)
			l.locked = true
			return nil
		}

		// Check for existing lock (PreconditionFailed) and handle accordingly.
		if !isPreconditionFailed(err) {
			return fmt.Errorf("failed to acquire orchestration lock: %w", err)
		}

		existing, err := l.Client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(l.Bucket),
			Key:    aws.String(l.key()),
		})
		if err != nil {
			return fmt.Errorf("lock exists but cannot be inspected: %w", err)
		}

		meta := normalizeMetadata(existing.Metadata)
		createdAt, _ := time.Parse(time.RFC3339, meta["timestamp"])
		age := time.Since(createdAt)

		if age > l.TTL {
			fmt.Printf("⚠️  Stale lock detected for %s (age %s) — releasing\n", l.Env, age.Round(time.Second))
			_, _ = l.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(l.Bucket),
				Key:    aws.String(l.key()),
			})
			if !force && wait {
				// After releasing, force a short wait before retry.
				select {
				case <-time.After(100 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			continue
		}

		if wait {
			fmt.Printf("🔁 Waiting for orchestration lock (held by %s since %s)\n", meta["owner"], createdAt.Format(time.RFC3339))
			select {
			case <-time.After(l.PollInterval):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return &LockedError{
			Env:       l.Env,
			Owner:     meta["owner"],
			Command:   meta["command"],
			Timestamp: createdAt,
		}
	}
}

// Release deletes the orchestration lock object from S3.
func (l *OrchestrationLock) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.locked {
		return nil
	}

	_, err := l.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(l.Bucket),
		Key:    aws.String(l.key()),
	})
	if err != nil {
		return fmt.Errorf("failed to release orchestration lock: %w", err)
	}

	l.locked = false
	fmt.Printf("🔓 Released orchestration lock for %s\n", l.Env)
	return nil
}

func isPreconditionFailed(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "preconditionfailed")
}

func normalizeMetadata(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return map[string]string{}
	}

	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[strings.ToLower(k)] = v
	}
	return out
}

func defaultOwner() string {
	if v := os.Getenv("CI_JOB_NAME"); v != "" {
		return v
	}
	if v := os.Getenv("GITHUB_RUN_ID"); v != "" {
		return "github-run-" + v
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

func ifNoneMatchOption(value string) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Serialize.Add(&ifNoneMatchMiddleware{value: value}, middleware.After)
	}
}

type ifNoneMatchMiddleware struct {
	value string
}

func (m *ifNoneMatchMiddleware) ID() string { return "IfNoneMatchHeader" }

func (m *ifNoneMatchMiddleware) HandleSerialize(ctx context.Context, in middleware.SerializeInput, next middleware.SerializeHandler) (middleware.SerializeOutput, middleware.Metadata, error) {
	if req, ok := in.Request.(*smithyhttp.Request); ok {
		req.Header.Set("If-None-Match", m.value)
	}
	return next.HandleSerialize(ctx, in)
}
