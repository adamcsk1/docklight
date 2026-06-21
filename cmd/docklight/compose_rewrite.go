package main

import (
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func rewriteComposeRelativeBinds(content []byte, hostStackPath, containerStackPath string) ([]byte, bool, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, false, err
	}
	changed := rewriteRelativeBindsNode(&root, hostStackPath, containerStackPath)
	if !changed {
		return content, false, nil
	}
	out, err := yaml.Marshal(&root)
	return out, true, err
}

func rewriteRelativeBindsNode(node *yaml.Node, hostStackPath, containerStackPath string) bool {
	if node == nil {
		return false
	}
	changed := false
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			value := node.Content[i+1]
			if key.Value == "volumes" && value.Kind == yaml.SequenceNode {
				for _, item := range value.Content {
					if rewriteVolumeEntry(item, hostStackPath, containerStackPath) {
						changed = true
					}
				}
			}
			if rewriteRelativeBindsNode(value, hostStackPath, containerStackPath) {
				changed = true
			}
		}
		return changed
	}
	for _, child := range node.Content {
		if rewriteRelativeBindsNode(child, hostStackPath, containerStackPath) {
			changed = true
		}
	}
	return changed
}

func rewriteVolumeEntry(node *yaml.Node, hostStackPath, containerStackPath string) bool {
	if node.Kind == yaml.ScalarNode {
		source, rest, ok := splitVolumeSpec(node.Value)
		if ok {
			if rewritten, ok := rewriteBindSource(source, hostStackPath, containerStackPath); ok {
				node.Value = rewritten + rest
				return true
			}
		}
		return false
	}
	if node.Kind != yaml.MappingNode {
		return false
	}
	typeNode := mappingValue(node, "type")
	sourceNode := mappingValue(node, "source")
	if sourceNode == nil {
		sourceNode = mappingValue(node, "src")
	}
	if sourceNode == nil {
		return false
	}
	if typeNode != nil && !strings.EqualFold(typeNode.Value, "bind") {
		return false
	}
	if rewritten, ok := rewriteBindSource(sourceNode.Value, hostStackPath, containerStackPath); ok {
		sourceNode.Value = rewritten
		return true
	}
	return false
}

func rewriteBindSource(source, hostStackPath, containerStackPath string) (string, bool) {
	if isRelativeBindSource(source) {
		return filepath.ToSlash(filepath.Join(hostStackPath, filepath.FromSlash(strings.TrimPrefix(source, "./")))), true
	}
	if containerStackPath == "" {
		return "", false
	}
	rel, err := filepath.Rel(filepath.FromSlash(containerStackPath), filepath.FromSlash(source))
	if err != nil || pathEscapesRoot(rel) {
		return "", false
	}
	if rel == "." {
		return filepath.ToSlash(hostStackPath), true
	}
	return filepath.ToSlash(filepath.Join(hostStackPath, rel)), true
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func splitVolumeSpec(value string) (string, string, bool) {
	if value == "" {
		return "", "", false
	}
	idx := strings.Index(value, ":")
	if idx <= 0 {
		return "", "", false
	}
	return value[:idx], value[idx:], true
}

func isRelativeBindSource(source string) bool {
	return source == "." || strings.HasPrefix(source, "./")
}
