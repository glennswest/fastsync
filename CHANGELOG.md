# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.4] - 2026-01-08

### Fixed
- Blob verification now uses HEAD requests to reliably detect missing blobs
- Post-write verification uses light manifest check to avoid false failures
- Fixes issue where Quay GC could delete blobs but old verification missed it

## [0.2.3] - 2026-01-08

### Fixed
- Removed Windows binary download from mirror-local.sh (not included in container images)

## [0.2.2] - 2026-01-07

### Added
- Retry logic for fetch operations with exponential backoff
- Improved UI feedback during sync operations

## [0.2.1] - 2026-01-07

### Added
- Auto-sync feature for OpenShift releases
- Improved dashboard stats and OpenShift version fetching

## [0.2.0] - 2026-01-07

### Added
- Web GUI for managing sync operations
- Push images by component tag instead of digest
- Layer blob verification before skipping images
- mirror-local.sh script for registry server deployment

### Fixed
- Transaction conflict handling improvements
- macOS download URL in mirror-local.sh
- Preserve image-references annotations during rewrite
- Preserve manifest annotations when rewriting release image

## [0.1.0] - 2026-01-06

### Added
- Initial release of fastsync
- Fast parallel mirroring of OpenShift release images
- Configurable worker pools for images and blobs
- Pull secret authentication support
- Insecure registry support
- Skip existing images (efficient resume)
- Image verification for sync and skip operations
- Retry logic with exponential backoff for failed images
- Rewrite image references in release image to point to destination registry
- Version information with `-v` flag
- Cross-platform builds for Linux (x86_64, ARM64, ARM), macOS (Intel, Apple Silicon)
