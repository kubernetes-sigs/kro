package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type ResourceGraphDefinitionLoadResult struct {
	Path string
	RGD  *v1alpha1.ResourceGraphDefinition
	Err  error
}

// collectYAMLFiles returns a list of YAML file paths from the given path.
// If path is a file, it returns a single-element slice.
// If path is a directory, it returns all .yaml and .yml files in the directory (non-recursive).
func collectYAMLFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to access path: %w", err)
	}

	if !info.IsDir() {
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil, fmt.Errorf("file %q must have a .yaml or .yml extension", path)
		}
		return []string{path}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext == ".yaml" || ext == ".yml" {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}

	sort.Strings(files)
	return files, nil
}

// LoadResourceGraphDefinitionsDetailed loads ResourceGraphDefinition resources from a file or directory,
// returning per-file results (including parse errors) so callers can continue on failure.
// Only errors related to accessing the path (stat/readdir) are returned directly.
func LoadResourceGraphDefinitionsDetailed(path string) ([]ResourceGraphDefinitionLoadResult, error) {
	files, err := collectYAMLFiles(path)
	if err != nil {
		return nil, err
	}

	results := make([]ResourceGraphDefinitionLoadResult, 0, len(files))
	for _, file := range files {
		rgd, loadErr := loadResourceGraphDefinitionFile(file)
		results = append(results, ResourceGraphDefinitionLoadResult{Path: file, RGD: rgd, Err: loadErr})
	}

	return results, nil
}

// LoadResourceGraphDefinitions loads ResourceGraphDefinition resources from a file or directory.
// If path is a file, it loads exactly that file.
// If path is a directory, it loads all .yaml and .yml files in the directory (non-recursive).
func LoadResourceGraphDefinitions(path string) ([]*v1alpha1.ResourceGraphDefinition, error) {
	results, err := LoadResourceGraphDefinitionsDetailed(path)
	if err != nil {
		return nil, err
	}

	loaded := make([]*v1alpha1.ResourceGraphDefinition, 0, len(results))
	for _, result := range results {
		if result.Err != nil {
			return nil, fmt.Errorf("failed to load %q: %w", result.Path, result.Err)
		}
		loaded = append(loaded, result.RGD)
	}

	return loaded, nil
}

func loadResourceGraphDefinitionFile(path string) (*v1alpha1.ResourceGraphDefinition, error) {
	data, err := loadFile(path)
	if err != nil {
		return nil, err
	}

	var rgd v1alpha1.ResourceGraphDefinition
	if err := yaml.Unmarshal(data, &rgd); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ResourceGraphDefinition: %w", err)
	}

	// Verify it's actually an RGD.
	if rgd.Kind != "ResourceGraphDefinition" {
		return nil, fmt.Errorf("expected kind ResourceGraphDefinition, got %s", rgd.Kind)
	}

	return &rgd, nil
}

// LoadInstance loads a single Kubernetes resource instance from a YAML file.
// The resource is returned as an *unstructured.Unstructured.
func LoadInstance(path string) (*unstructured.Unstructured, error) {
	data, err := loadFile(path)
	if err != nil {
		return nil, err
	}

	var obj map[string]any
	if err := yaml.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("failed to unmarshal instance: %w", err)
	}

	if len(obj) == 0 {
		return nil, fmt.Errorf("empty YAML document")
	}

	return &unstructured.Unstructured{Object: obj}, nil
}

// LoadInstances loads Kubernetes resource instances from a file or directory.
// If path is a file, it loads exactly that file.
// If path is a directory, it loads all .yaml and .yml files in the directory (non-recursive).
func LoadInstances(path string) ([]*unstructured.Unstructured, error) {
	files, err := collectYAMLFiles(path)
	if err != nil {
		return nil, err
	}

	loaded := make([]*unstructured.Unstructured, 0, len(files))
	for _, file := range files {
		obj, err := LoadInstance(file)
		if err != nil {
			return nil, fmt.Errorf("failed to load %q: %w", file, err)
		}
		loaded = append(loaded, obj)
	}

	return loaded, nil
}

// loadFile reads a YAML file and returns its content as a byte slice.
func loadFile(path string) ([]byte, error) {
	// Validate file exists and is accessible
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to access file: %w", err)
	}

	if info.IsDir() {
		return nil, fmt.Errorf("path %q is a directory, provide a path to a configuration file (.yaml or .yml)", path)
	}

	// Check file extension
	ext := filepath.Ext(path)
	if ext != ".yaml" && ext != ".yml" {
		return nil, fmt.Errorf("file %q must have a .yaml or .yml extension", path)
	}

	// Read the YAML file
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	return content, nil
}
