package mirror

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"fastsync/pkg/release"
)

const (
	maxRetries    = 3
	retryBaseWait = 2 * time.Second
)

// Progress tracks mirror progress
type Progress struct {
	Total     int
	Completed int64
	Failed    int64
	Skipped   int64
}

// Result represents the result of mirroring a single image
type Result struct {
	Component string
	Source    string
	Dest      string
	Skipped   bool
	Error     error
}

// Engine handles parallel image mirroring
type Engine struct {
	Workers      int
	BlobWorkers  int // parallel blob uploads per image
	DestRegistry string
	DestRepo     string
	Keychain     authn.Keychain
	Insecure     bool
	Progress     *Progress
	OnResult     func(Result)
}

// NewEngine creates a new mirror engine
func NewEngine(workers, blobWorkers int, destRegistry, destRepo string, keychain authn.Keychain, insecure bool) *Engine {
	if blobWorkers < 1 {
		blobWorkers = 4
	}
	return &Engine{
		Workers:      workers,
		BlobWorkers:  blobWorkers,
		DestRegistry: destRegistry,
		DestRepo:     destRepo,
		Keychain:     keychain,
		Insecure:     insecure,
		Progress:     &Progress{},
	}
}

// Mirror copies all images from a release to the destination registry
func (e *Engine) Mirror(ctx context.Context, rel *release.Release) error {
	e.Progress.Total = len(rel.Components) + 1 // +1 for release image itself

	// Create work channel
	work := make(chan release.ComponentImage, len(rel.Components))
	results := make(chan Result, len(rel.Components))

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < e.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.worker(ctx, work, results)
		}()
	}

	// Queue all component images
	go func() {
		for _, comp := range rel.Components {
			select {
			case work <- comp:
			case <-ctx.Done():
				return
			}
		}
		close(work)
	}()

	// Collect results in background
	go func() {
		wg.Wait()
		close(results)
	}()

	// Process results
	var errors []error
	for result := range results {
		if e.OnResult != nil {
			e.OnResult(result)
		}
		if result.Error != nil {
			errors = append(errors, fmt.Errorf("%s: %w", result.Component, result.Error))
		}
	}

	// Mirror the release image itself
	if err := e.mirrorReleaseImage(ctx, rel); err != nil {
		errors = append(errors, fmt.Errorf("release image: %w", err))
		fmt.Printf("[release] FAILED: %v\n", err)
	} else {
		fmt.Printf("[release] OK (image references rewritten)\n")
	}
	atomic.AddInt64(&e.Progress.Completed, 1)

	if len(errors) > 0 {
		return fmt.Errorf("%d images failed to mirror", len(errors))
	}
	return nil
}

func (e *Engine) worker(ctx context.Context, work <-chan release.ComponentImage, results chan<- Result) {
	for comp := range work {
		select {
		case <-ctx.Done():
			return
		default:
		}

		result := e.mirrorImage(ctx, comp)
		results <- result

		if result.Error != nil {
			atomic.AddInt64(&e.Progress.Failed, 1)
		} else if result.Skipped {
			atomic.AddInt64(&e.Progress.Skipped, 1)
		}
		atomic.AddInt64(&e.Progress.Completed, 1)
	}
}

func (e *Engine) mirrorImage(ctx context.Context, comp release.ComponentImage) Result {
	result := Result{
		Component: comp.Name,
		Source:    comp.Image,
	}

	// Parse source reference
	srcRef, err := name.ParseReference(comp.Image)
	if err != nil {
		result.Error = fmt.Errorf("parsing source reference: %w", err)
		return result
	}

	// Build destination reference - preserve the digest
	var destRefStr string
	if digest, ok := srcRef.(name.Digest); ok {
		destRefStr = fmt.Sprintf("%s/%s@%s", e.DestRegistry, e.DestRepo, digest.DigestStr())
	} else {
		destRefStr = fmt.Sprintf("%s/%s:%s", e.DestRegistry, e.DestRepo, comp.Name)
	}
	result.Dest = destRefStr

	destRef, err := name.ParseReference(destRefStr)
	if err != nil {
		result.Error = fmt.Errorf("parsing dest reference: %w", err)
		return result
	}

	// Build options
	srcOpts := []remote.Option{remote.WithAuthFromKeychain(e.Keychain), remote.WithContext(ctx)}
	destOpts := []remote.Option{
		remote.WithAuthFromKeychain(e.Keychain),
		remote.WithContext(ctx),
		remote.WithJobs(e.BlobWorkers), // parallel blob uploads
	}

	if e.Insecure {
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		srcOpts = append(srcOpts, remote.WithTransport(transport))
		destOpts = append(destOpts, remote.WithTransport(transport))
	}

	// Check if image already exists at destination
	if e.imageExists(ctx, destRef, destOpts) {
		result.Skipped = true
		return result
	}

	// Fetch source image
	img, err := remote.Image(srcRef, srcOpts...)
	if err != nil {
		result.Error = fmt.Errorf("fetching source: %w", err)
		return result
	}

	// Push to destination with parallel blob uploads
	if err := remote.Write(destRef, img, destOpts...); err != nil {
		result.Error = fmt.Errorf("writing to dest: %w", err)
		return result
	}

	return result
}

func (e *Engine) mirrorReleaseImage(ctx context.Context, rel *release.Release) error {
	srcRef, err := name.ParseReference(rel.SourceRef)
	if err != nil {
		return fmt.Errorf("parsing source: %w", err)
	}

	destRefStr := fmt.Sprintf("%s/%s:%s-x86_64", e.DestRegistry, e.DestRepo, rel.Version)
	destRef, err := name.ParseReference(destRefStr)
	if err != nil {
		return fmt.Errorf("parsing dest: %w", err)
	}

	srcOpts := []remote.Option{remote.WithAuthFromKeychain(e.Keychain), remote.WithContext(ctx)}
	destOpts := []remote.Option{
		remote.WithAuthFromKeychain(e.Keychain),
		remote.WithContext(ctx),
		remote.WithJobs(e.BlobWorkers),
	}
	if e.Insecure {
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		srcOpts = append(srcOpts, remote.WithTransport(transport))
		destOpts = append(destOpts, remote.WithTransport(transport))
	}

	img, err := remote.Image(srcRef, srcOpts...)
	if err != nil {
		return fmt.Errorf("fetching: %w", err)
	}

	// Rewrite image references to point to destination registry
	rewrittenImg, err := release.CreateRewrittenReleaseImage(img, e.DestRegistry, e.DestRepo)
	if err != nil {
		return fmt.Errorf("rewriting image references: %w", err)
	}

	return remote.Write(destRef, rewrittenImg, destOpts...)
}

func (e *Engine) imageExists(ctx context.Context, ref name.Reference, opts []remote.Option) bool {
	_, err := remote.Head(ref, opts...)
	return err == nil
}

// MultiKeychain combines multiple keychains for authentication
type MultiKeychain struct {
	keychains []authn.Keychain
}

func NewMultiKeychain(keychains ...authn.Keychain) *MultiKeychain {
	return &MultiKeychain{keychains: keychains}
}

func (m *MultiKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	for _, kc := range m.keychains {
		auth, err := kc.Resolve(res)
		if err == nil && auth != authn.Anonymous {
			return auth, nil
		}
	}
	return authn.Anonymous, nil
}

// PullSecretKeychain implements authentication from a Docker pull secret JSON file
type PullSecretKeychain struct {
	auths map[string]authn.AuthConfig
}

// NewPullSecretKeychain creates a keychain from a pull secret JSON file
func NewPullSecretKeychain(auths map[string]authn.AuthConfig) *PullSecretKeychain {
	return &PullSecretKeychain{auths: auths}
}

func (k *PullSecretKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	registry := res.RegistryStr()

	// Try exact match first
	if auth, ok := k.auths[registry]; ok {
		return authn.FromConfig(authn.AuthConfig{
			Username: auth.Username,
			Password: auth.Password,
			Auth:     auth.Auth,
		}), nil
	}

	// Try with https:// prefix
	if auth, ok := k.auths["https://"+registry]; ok {
		return authn.FromConfig(authn.AuthConfig{
			Username: auth.Username,
			Password: auth.Password,
			Auth:     auth.Auth,
		}), nil
	}

	return authn.Anonymous, nil
}
