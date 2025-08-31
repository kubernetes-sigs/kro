// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package login

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

type LoginConfig struct {
	registry string
	username string
	password string
}

type RegistryAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

type ConfigFile struct {
	Auths map[string]RegistryAuth `json:"auths"`
}

var loginConfig = &LoginConfig{}

func init() {
	loginCmd.PersistentFlags().StringVarP(&loginConfig.registry,
		"registry", "r", "",
		"Registry server to log in to (e.g., 'ghcr.io', 'docker.io')",
	)
	loginCmd.PersistentFlags().StringVarP(&loginConfig.username,
		"username", "u", "",
		"Username for the registry",
	)
	loginCmd.PersistentFlags().StringVarP(&loginConfig.password,
		"password", "p", "",
		"Password for the registry (not recommended, use interactive mode)",
	)
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Log in to a container registry",
	Long: "The login command authenticates with a container registry and stores the credentials " +
		"in the KRO configuration file for future use.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if loginConfig.registry == "" {
			return fmt.Errorf("remote reference is required, please use the --ref flag")
		}

		registry, err := normalizeRegistry(loginConfig.registry)
		if err != nil {
			return fmt.Errorf("invalid registry URL: %w", err)
		}
		loginConfig.registry = registry

		if loginConfig.username == "" {
			fmt.Print("Username: ")
			reader := bufio.NewReader(os.Stdin)
			username, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("failed to read username: %w", err)
			}
			loginConfig.username = strings.TrimSpace(username)
		}

		if loginConfig.password == "" {
			fmt.Print("Password: ")
			bytePassword, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				return fmt.Errorf("failed to read password: %w", err)
			}
			loginConfig.password = string(bytePassword)
		}

		if loginConfig.username == "" || loginConfig.password == "" {
			return fmt.Errorf("username and password are required")
		}

		if err := saveCredentials(loginConfig.registry, loginConfig.username, loginConfig.password); err != nil {
			return fmt.Errorf("failed to save credentials: %w", err)
		}

		fmt.Println("Login Succeeded! Credentials saved to", getConfigPath())
		return nil
	},
}

func normalizeRegistry(registry string) (string, error) {
	switch registry {
	case "docker.io", "index.docker.io":
		return "https://index.docker.io/v1/", nil
	case "ghcr.io":
		return "ghcr.io", nil
	}

	if !strings.Contains(registry, "://") {
		registry = "https://" + registry
	}

	u, err := url.Parse(registry)
	if err != nil {
		return "", err
	}

	return u.Host, nil
}

func saveCredentials(registry, username, password string) error {
	configPath := getConfigPath()
	configDir := filepath.Dir(configPath)

	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	config := &ConfigFile{
		Auths: make(map[string]RegistryAuth),
	}

	if _, err := os.Stat(configPath); err == nil {
		configBytes, err := os.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("failed to read existing config: %w", err)
		}

		if err := json.Unmarshal(configBytes, config); err != nil {
			return fmt.Errorf("failed to parse existing config: %w", err)
		}
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))

	config.Auths[registry] = RegistryAuth{
		Username: username,
		Password: string(hashedPassword),
		Auth:     auth,
	}

	configBytes, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

func getConfigPath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".kro", "registry", "config.json")
	}
	return filepath.Join(homeDir, ".config", "kro", "registry", "config.json")
}

func AddLoginCommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(loginCmd)
}
