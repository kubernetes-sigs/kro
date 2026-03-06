//go:build ignore
// +build ignore

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	configFile    = "website/examples.config.yaml"
	outputBaseDir = "website/docs/examples"
)

// ExampleEntry represents a single example entry in the config
type ExampleEntry struct {
	Name            string     `yaml:"name"`
	Description     string     `yaml:"description"`
	SidebarPosition int        `yaml:"sidebar_position"`
	Files           []FilePath `yaml:"files"`
}

// FilePath represents a path entry in the files list
type FilePath struct {
	Path string `yaml:"path"`
}

// ExamplesConfig represents the entire configuration file
// The key is the category name (e.g., "kubernetes", "aws")
type ExamplesConfig map[string][]ExampleEntry

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Read the config file
	config, err := readConfig(configFile)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Process each category
	for category, examples := range config {
		fmt.Printf("Processing category: %s\n", category)

		for idx, example := range examples {
			sidebarPosition := example.SidebarPosition
			if sidebarPosition == 0 {
				sidebarPosition = idx + 1
			}

			for _, file := range example.Files {
				// Resolve the path relative to the config file location
				examplePath := filepath.Join(filepath.Dir(configFile), file.Path)

				if err := processExample(category, example, examplePath, sidebarPosition); err != nil {
					return fmt.Errorf("failed to process example %s: %w", example.Name, err)
				}
			}
		}
	}

	fmt.Println("Documentation generation completed successfully!")
	return nil
}

func readConfig(path string) (ExamplesConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", path, err)
	}

	var config ExamplesConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	return config, nil
}

func processExample(category string, example ExampleEntry, examplePath string, sidebarPosition int) error {
	fmt.Printf("  Processing example: %s (path: %s)\n", example.Name, examplePath)

	// Check if the example directory exists
	info, err := os.Stat(examplePath)
	if err != nil {
		return fmt.Errorf("example path does not exist: %s: %w", examplePath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("example path is not a directory: %s", examplePath)
	}

	// Read README.md
	readmePath := filepath.Join(examplePath, "README.md")
	readmeContent, err := os.ReadFile(readmePath)
	if err != nil {
		return fmt.Errorf("failed to read README.md at %s: %w", readmePath, err)
	}

	// Collect manifest files
	manifestFiles, err := collectManifestFiles(examplePath)
	if err != nil {
		return fmt.Errorf("failed to collect manifest files: %w", err)
	}

	// Generate markdown content
	markdownContent := generateMarkdown(string(readmeContent), manifestFiles, sidebarPosition)

	// Determine output path
	outputFileName := toKebabCase(example.Name) + ".md"
	outputDir := filepath.Join(outputBaseDir, category)
	outputPath := filepath.Join(outputDir, outputFileName)

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory %s: %w", outputDir, err)
	}

	// Write the markdown file
	if err := os.WriteFile(outputPath, []byte(markdownContent), 0644); err != nil {
		return fmt.Errorf("failed to write output file %s: %w", outputPath, err)
	}

	fmt.Printf("    Generated: %s\n", outputPath)
	return nil
}

// ManifestFile represents a manifest file with its name and content
type ManifestFile struct {
	Name    string
	Content string
}

func collectManifestFiles(dirPath string) ([]ManifestFile, error) {
	var manifestFiles []ManifestFile

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", dirPath, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			filePath := filepath.Join(dirPath, name)
			content, err := os.ReadFile(filePath)
			if err != nil {
				return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
			}

			manifestFiles = append(manifestFiles, ManifestFile{
				Name:    name,
				Content: string(content),
			})
		}
	}

	// Sort by filename for consistent output
	sort.Slice(manifestFiles, func(i, j int) bool {
		return manifestFiles[i].Name < manifestFiles[j].Name
	})

	return manifestFiles, nil
}

func generateMarkdown(readmeContent string, manifestFiles []ManifestFile, sidebarPosition int) string {
	var sb strings.Builder

	// Add frontmatter
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("sidebar_position: %d\n", sidebarPosition))
	sb.WriteString("---\n\n")

	// Add Tabs import for Docusaurus
	if len(manifestFiles) > 0 {
		sb.WriteString("import Tabs from '@theme/Tabs';\n")
		sb.WriteString("import TabItem from '@theme/TabItem';\n\n")
	}

	// Add README content
	sb.WriteString(readmeContent)

	// Add Manifest files in Tabs component
	if len(manifestFiles) > 0 {
		// Add Tab
		sb.WriteString("## Manifest files\n\n")
		sb.WriteString("\n\n<Tabs>\n")

		for _, manifestFile := range manifestFiles {
			// Generate a value identifier from filename (remove extension and special chars)
			valueID := strings.TrimSuffix(manifestFile.Name, filepath.Ext(manifestFile.Name))
			valueID = strings.ReplaceAll(valueID, ".", "-")

			sb.WriteString(fmt.Sprintf("  <TabItem value=\"%s\" label=\"%s\">\n\n", valueID, manifestFile.Name))
			sb.WriteString("```kro\n")
			sb.WriteString(manifestFile.Content)
			if !strings.HasSuffix(manifestFile.Content, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("```\n\n")
			sb.WriteString("  </TabItem>\n")
		}

		sb.WriteString("</Tabs>\n")
	}

	return sb.String()
}

// toKebabCase converts a string to kebab-case
// e.g., "SaaS Multi Tenant" -> "saas-multi-tenant"
func toKebabCase(s string) string {
	// Convert to lowercase
	s = strings.ToLower(s)

	// Replace spaces with hyphens
	s = strings.ReplaceAll(s, " ", "-")

	// Replace multiple hyphens with single hyphen
	re := regexp.MustCompile(`-+`)
	s = re.ReplaceAllString(s, "-")

	// Remove any characters that are not alphanumeric or hyphens
	re = regexp.MustCompile(`[^a-z0-9-]`)
	s = re.ReplaceAllString(s, "")

	// Trim leading and trailing hyphens
	s = strings.Trim(s, "-")

	return s
}
