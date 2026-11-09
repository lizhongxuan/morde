package mordesync

import (
	"fmt"
	"strings"
)

type manifestPathNode struct {
	path      string
	directory bool
	empty     bool
}

// ManifestPathValidator rejects file and directory trees that cannot be
// represented consistently on case-insensitive filesystems.
type ManifestPathValidator struct {
	nodes map[string]manifestPathNode
}

// Add validates and records one file path in the manifest tree.
func (v *ManifestPathValidator) Add(value string) error {
	if err := validRelativePath(value); err != nil {
		return fmt.Errorf("manifest path %q: %w", value, ErrInvalidManifest)
	}
	if v.nodes == nil {
		v.nodes = make(map[string]manifestPathNode)
	}

	parts := strings.Split(value, "/")
	var prefix string
	pending := make([]manifestPathNode, 0, len(parts))
	for index, part := range parts {
		if index > 0 {
			prefix += "/"
		}
		prefix += part
		directory := index < len(parts)-1
		key := strings.ToLower(prefix)
		if previous, exists := v.nodes[key]; exists {
			if previous.path != prefix || previous.directory != directory || !directory || previous.empty {
				return fmt.Errorf("manifest path %q conflicts with %q: %w", value, previous.path, ErrInvalidManifest)
			}
			continue
		}
		pending = append(pending, manifestPathNode{path: prefix, directory: directory})
	}
	for _, node := range pending {
		v.nodes[strings.ToLower(node.path)] = node
	}
	return nil
}

// AddEmptyDirectory records an explicit empty leaf directory in a manifest.
func (v *ManifestPathValidator) AddEmptyDirectory(value string) error {
	if err := validRelativePath(value); err != nil {
		return fmt.Errorf("manifest directory %q: %w", value, ErrInvalidManifest)
	}
	if v.nodes == nil {
		v.nodes = make(map[string]manifestPathNode)
	}
	parts := strings.Split(value, "/")
	var prefix string
	for index, part := range parts {
		if index > 0 {
			prefix += "/"
		}
		prefix += part
		pathKey := strings.ToLower(prefix)
		isLeaf := index == len(parts)-1
		if previous, exists := v.nodes[pathKey]; exists {
			if previous.path != prefix || !previous.directory || previous.empty {
				return fmt.Errorf("manifest directory %q conflicts with %q: %w", value, previous.path, ErrInvalidManifest)
			}
			if isLeaf {
				return fmt.Errorf("manifest directory %q is not empty: %w", value, ErrInvalidManifest)
			}
			continue
		}
		v.nodes[pathKey] = manifestPathNode{path: prefix, directory: true, empty: isLeaf}
	}
	return nil
}
