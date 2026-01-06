package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"fastsync/pkg/mirror"
	"fastsync/pkg/release"
)

const (
	defaultSourceRegistry = "quay.io/openshift-release-dev/ocp-release"
	defaultWorkers        = 16
)

// PullSecret represents the Docker auth config format
type PullSecret struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username,omitempty"`
		Password string `json:"password,omitempty"`
	} `json:"auths"`
}

func main() {
	var (
		version        = flag.String("version", "", "OpenShift version to mirror (e.g., 4.20.8)")
		workers        = flag.Int("workers", defaultWorkers, "Number of parallel image workers")
		blobWorkers    = flag.Int("blob-workers", 4, "Number of parallel blob uploads per image")
		pullSecretPath = flag.String("pull-secret", "", "Path to pull secret JSON file")
		destRegistry   = flag.String("dest-registry", "", "Destination registry (e.g., registry.gw.lo)")
		destRepo       = flag.String("dest-repo", "openshift/release", "Destination repository path")
		sourceRegistry = flag.String("source-registry", defaultSourceRegistry, "Source registry for release images")
		insecure       = flag.Bool("insecure", false, "Allow insecure registry connections")
	)
	flag.Parse()

	if *version == "" {
		fmt.Fprintln(os.Stderr, "Error: --version is required")
		flag.Usage()
		os.Exit(1)
	}
	if *destRegistry == "" {
		fmt.Fprintln(os.Stderr, "Error: --dest-registry is required")
		flag.Usage()
		os.Exit(1)
	}
	if *pullSecretPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --pull-secret is required")
		flag.Usage()
		os.Exit(1)
	}

	// Load pull secret
	keychain, err := loadPullSecret(*pullSecretPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading pull secret: %v\n", err)
		os.Exit(1)
	}

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nInterrupted, stopping...")
		cancel()
	}()

	startTime := time.Now()
	fmt.Printf("=== FastSync OpenShift %s ===\n", *version)
	fmt.Printf("Source: %s:%s-x86_64\n", *sourceRegistry, *version)
	fmt.Printf("Dest:   %s/%s:%s-x86_64\n", *destRegistry, *destRepo, *version)
	fmt.Printf("Workers: %d images x %d blobs = %d concurrent\n", *workers, *blobWorkers, *workers**blobWorkers)
	fmt.Println()

	// Fetch release info
	fmt.Print("Fetching release manifest... ")
	rel, err := release.GetRelease(*sourceRegistry, *version, keychain, *insecure)
	if err != nil {
		fmt.Printf("FAILED\n")
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK (%d images)\n", len(rel.Components))

	// Create mirror engine
	engine := mirror.NewEngine(*workers, *blobWorkers, *destRegistry, *destRepo, keychain, *insecure)

	// Progress callback
	engine.OnResult = func(r mirror.Result) {
		status := "OK"
		if r.Error != nil {
			status = fmt.Sprintf("FAILED: %v", r.Error)
		} else if r.Skipped {
			status = "SKIPPED (exists)"
		}
		fmt.Printf("[%d/%d] %s: %s\n",
			engine.Progress.Completed+1,
			engine.Progress.Total,
			r.Component,
			status)
	}

	// Run mirror
	fmt.Println()
	fmt.Println("Mirroring images...")
	if err := engine.Mirror(ctx, rel); err != nil {
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		os.Exit(1)
	}

	// Summary
	elapsed := time.Since(startTime)
	fmt.Println()
	fmt.Println("=== Complete ===")
	fmt.Printf("Total:    %d images\n", engine.Progress.Total)
	fmt.Printf("Copied:   %d\n", engine.Progress.Completed-engine.Progress.Skipped-engine.Progress.Failed)
	fmt.Printf("Skipped:  %d (already exist)\n", engine.Progress.Skipped)
	fmt.Printf("Failed:   %d\n", engine.Progress.Failed)
	fmt.Printf("Duration: %s\n", elapsed.Round(time.Second))
	fmt.Printf("\nRelease: %s/%s:%s-x86_64\n", *destRegistry, *destRepo, *version)
}

func loadPullSecret(path string) (*mirror.PullSecretKeychain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	var ps PullSecret
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}

	auths := make(map[string]authn.AuthConfig)
	for registry, auth := range ps.Auths {
		cfg := authn.AuthConfig{}

		if auth.Auth != "" {
			// Decode base64 auth string
			decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
			if err == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				if len(parts) == 2 {
					cfg.Username = parts[0]
					cfg.Password = parts[1]
				}
			}
			cfg.Auth = auth.Auth
		}
		if auth.Username != "" {
			cfg.Username = auth.Username
		}
		if auth.Password != "" {
			cfg.Password = auth.Password
		}

		auths[registry] = cfg
	}

	return mirror.NewPullSecretKeychain(auths), nil
}
