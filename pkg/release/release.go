package release

import (
	"archive/tar"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// ImageTag represents a single tag in the image-references
type ImageTag struct {
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations,omitempty"`
	From        struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"from"`
	Generation      interface{}            `json:"generation,omitempty"`
	ImportPolicy    map[string]interface{} `json:"importPolicy,omitempty"`
	ReferencePolicy map[string]string      `json:"referencePolicy,omitempty"`
}

// ImageReferences is the structure of the image-references file in the release
type ImageReferences struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Metadata   struct {
		Name              string            `json:"name"`
		CreationTimestamp string            `json:"creationTimestamp,omitempty"`
		Annotations       map[string]string `json:"annotations,omitempty"`
	} `json:"metadata"`
	Spec struct {
		LookupPolicy struct {
			Local bool `json:"local"`
		} `json:"lookupPolicy,omitempty"`
		Tags []ImageTag `json:"tags"`
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

// RewriteImageReferences rewrites image references to point to the destination registry
// while preserving all metadata including annotations (which contain version info)
func RewriteImageReferences(refs *ImageReferences, destRegistry, destRepo string) *ImageReferences {
	rewritten := &ImageReferences{
		Kind:       refs.Kind,
		APIVersion: refs.APIVersion,
		Metadata:   refs.Metadata,
	}
	rewritten.Spec.LookupPolicy = refs.Spec.LookupPolicy
	rewritten.Spec.Tags = make([]ImageTag, len(refs.Spec.Tags))

	for i, tag := range refs.Spec.Tags {
		// Copy all fields from the original tag
		rewritten.Spec.Tags[i] = ImageTag{
			Name:            tag.Name,
			Annotations:     tag.Annotations,
			Generation:      tag.Generation,
			ImportPolicy:    tag.ImportPolicy,
			ReferencePolicy: tag.ReferencePolicy,
		}
		rewritten.Spec.Tags[i].From.Kind = tag.From.Kind

		// Rewrite the image reference to point to destination registry
		// Format: registry/repo@sha256:digest or registry/repo:tag
		origImage := tag.From.Name
		if idx := strings.LastIndex(origImage, "@"); idx != -1 {
			digest := origImage[idx+1:]
			rewritten.Spec.Tags[i].From.Name = fmt.Sprintf("%s/%s@%s", destRegistry, destRepo, digest)
		} else {
			// Fallback: use component name as tag
			rewritten.Spec.Tags[i].From.Name = fmt.Sprintf("%s/%s:%s", destRegistry, destRepo, tag.Name)
		}
	}

	return rewritten
}

// CreateRewrittenReleaseImage creates a new release image with rewritten image references
func CreateRewrittenReleaseImage(img v1.Image, destRegistry, destRepo string) (v1.Image, error) {
	// Extract the original image references
	refs, err := extractImageReferences(img)
	if err != nil {
		return nil, fmt.Errorf("extracting image references: %w", err)
	}

	// Rewrite the references
	rewritten := RewriteImageReferences(refs, destRegistry, destRepo)

	// Serialize the rewritten references
	rewrittenJSON, err := json.MarshalIndent(rewritten, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling rewritten references: %w", err)
	}

	// Get all layers from the original image
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("getting layers: %w", err)
	}

	// Get the original config
	configFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("getting config: %w", err)
	}

	// Find and replace the layer containing image-references
	var newLayers []v1.Layer
	replacedIdx := -1

	for i, layer := range layers {
		hasRefs, err := layerContainsImageReferences(layer)
		if err != nil {
			return nil, fmt.Errorf("checking layer: %w", err)
		}
		if hasRefs && replacedIdx == -1 {
			// Create a new layer with modified image-references
			newLayer, err := createModifiedLayer(layer, rewrittenJSON)
			if err != nil {
				return nil, fmt.Errorf("creating modified layer: %w", err)
			}
			newLayers = append(newLayers, newLayer)
			replacedIdx = i
		} else {
			newLayers = append(newLayers, layer)
		}
	}

	if replacedIdx == -1 {
		return nil, fmt.Errorf("image-references layer not found")
	}

	// Update the config's DiffID for the replaced layer
	newDiffID, err := newLayers[replacedIdx].DiffID()
	if err != nil {
		return nil, fmt.Errorf("getting new layer DiffID: %w", err)
	}
	newConfig := configFile.DeepCopy()
	if replacedIdx < len(newConfig.RootFS.DiffIDs) {
		newConfig.RootFS.DiffIDs[replacedIdx] = newDiffID
	}

	// Get original media type
	mediaType, err := img.MediaType()
	if err != nil {
		return nil, fmt.Errorf("getting media type: %w", err)
	}

	// Get original manifest to preserve annotations
	origManifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("getting manifest: %w", err)
	}

	// Build the new image: start with empty, add layers, then set config
	newImg := mutate.MediaType(empty.Image, mediaType)

	// Append layers
	newImg, err = mutate.AppendLayers(newImg, newLayers...)
	if err != nil {
		return nil, fmt.Errorf("appending layers: %w", err)
	}

	// Set the updated config
	newImg, err = mutate.ConfigFile(newImg, newConfig)
	if err != nil {
		return nil, fmt.Errorf("setting config: %w", err)
	}

	// Preserve original manifest annotations (contains displayVersions, etc.)
	if origManifest.Annotations != nil && len(origManifest.Annotations) > 0 {
		newImg = mutate.Annotations(newImg, origManifest.Annotations).(v1.Image)
	}

	return newImg, nil
}

func layerContainsImageReferences(layer v1.Layer) (bool, error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return false, err
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
		if header.Name == "release-manifests/image-references" {
			return true, nil
		}
	}
	return false, nil
}

func createModifiedLayer(origLayer v1.Layer, newImageRefs []byte) (v1.Layer, error) {
	// Read the original layer
	rc, err := origLayer.Uncompressed()
	if err != nil {
		return nil, fmt.Errorf("uncompressing layer: %w", err)
	}
	defer rc.Close()

	// Create a new tar archive with modified content
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tr := tar.NewReader(rc)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}

		if header.Name == "release-manifests/image-references" {
			// Write the modified image-references
			newHeader := &tar.Header{
				Name:    header.Name,
				Mode:    header.Mode,
				Size:    int64(len(newImageRefs)),
				ModTime: header.ModTime,
			}
			if err := tw.WriteHeader(newHeader); err != nil {
				return nil, fmt.Errorf("writing header: %w", err)
			}
			if _, err := tw.Write(newImageRefs); err != nil {
				return nil, fmt.Errorf("writing content: %w", err)
			}
		} else {
			// Copy the original file
			if err := tw.WriteHeader(header); err != nil {
				return nil, fmt.Errorf("writing header: %w", err)
			}
			if header.Size > 0 {
				if _, err := io.Copy(tw, tr); err != nil {
					return nil, fmt.Errorf("copying content: %w", err)
				}
			}
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing tar: %w", err)
	}

	// Create a layer from the tar
	layer, err := tarball.LayerFromReader(&buf)
	if err != nil {
		return nil, fmt.Errorf("creating layer: %w", err)
	}

	return layer, nil
}
