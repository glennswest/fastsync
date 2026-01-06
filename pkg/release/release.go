package release

import (
	"archive/tar"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ImageReferences is the structure of the image-references file in the release
type ImageReferences struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Tags []struct {
			Name string `json:"name"`
			From struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"from"`
		} `json:"tags"`
	} `json:"spec"`
}

// ComponentImage represents an image to be mirrored
type ComponentImage struct {
	Name  string // Component name (e.g., "etcd", "kubernetes")
	Image string // Full image reference with digest
}

// Release represents a parsed OpenShift release
type Release struct {
	Version    string
	SourceRef  string
	Components []ComponentImage
}

// InsecureTransport returns an HTTP transport that skips TLS verification
func InsecureTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
}

// GetRelease fetches and parses an OpenShift release image
func GetRelease(sourceRegistry, version string, keychain authn.Keychain, insecure bool) (*Release, error) {
	releaseRef := fmt.Sprintf("%s:%s-x86_64", sourceRegistry, version)

	ref, err := name.ParseReference(releaseRef)
	if err != nil {
		return nil, fmt.Errorf("parsing release reference: %w", err)
	}

	opts := []remote.Option{remote.WithAuthFromKeychain(keychain)}
	if insecure {
		opts = append(opts, remote.WithTransport(InsecureTransport()))
	}

	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("fetching release image: %w", err)
	}

	// Extract image-references from the release image
	imageRefs, err := extractImageReferences(img)
	if err != nil {
		return nil, fmt.Errorf("extracting image references: %w", err)
	}

	release := &Release{
		Version:   version,
		SourceRef: releaseRef,
	}

	for _, tag := range imageRefs.Spec.Tags {
		if tag.From.Kind == "DockerImage" {
			release.Components = append(release.Components, ComponentImage{
				Name:  tag.Name,
				Image: tag.From.Name,
			})
		}
	}

	return release, nil
}

func extractImageReferences(img v1.Image) (*ImageReferences, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("getting layers: %w", err)
	}

	// Search through all layers for image-references file
	for _, layer := range layers {
		rc, err := layer.Uncompressed()
		if err != nil {
			continue
		}

		refs, err := findImageReferencesInTar(rc)
		rc.Close()
		if err == nil && refs != nil {
			return refs, nil
		}
	}

	return nil, fmt.Errorf("image-references not found in release image")
}

func findImageReferencesInTar(r io.Reader) (*ImageReferences, error) {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		if header.Name == "release-manifests/image-references" {
			var refs ImageReferences
			if err := json.NewDecoder(tr).Decode(&refs); err != nil {
				return nil, err
			}
			return &refs, nil
		}
	}
	return nil, nil
}
