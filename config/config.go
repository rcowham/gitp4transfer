package config

import (
	"fmt"
	"os"
	"regexp"

	yaml "gopkg.in/yaml.v2"
)

const DefaultDepot = "import"
const DefaultBranch = "main"

type BranchMapping struct {
	Name   string `yaml:"name"`   // Regex for branch
	Prefix string `yaml:"prefix"` // Prefix to prepend to matching branches
}

// Config for gitp4transfer
type Config struct {
	ImportDepot    string          `yaml:"import_depot"`
	ImportPath     string          `yaml:"import_path"`
	DefaultBranch  string          `yaml:"default_branch"`
	BranchMappings []BranchMapping `yaml:"branch_mappings"`
}

// Unmarshal the config
func Unmarshal(config []byte) (*Config, error) {
	// Default values specified here
	cfg := &Config{
		ImportDepot:   "import",
		DefaultBranch: "main",
	}
	err := yaml.Unmarshal(config, cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %v. make sure to use 'single quotes' around strings with special characters (like match patterns)", err.Error())
	}
	err = cfg.validate()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadConfigFile - loads config file
func LoadConfigFile(filename string) (*Config, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to load %v: %v", filename, err.Error())
	}
	cfg, err := LoadConfigString(content)
	if err != nil {
		return nil, fmt.Errorf("failed to load %v: %v", filename, err.Error())
	}
	return cfg, nil
}

// LoadConfigString - loads a string
func LoadConfigString(content []byte) (*Config, error) {
	cfg, err := Unmarshal([]byte(content))
	return cfg, err
}

func (c *Config) validate() error {
	if len(c.BranchMappings) > 0 {
		for _, m := range c.BranchMappings {
			if _, err := regexp.Compile(m.Name); err != nil {
				return fmt.Errorf("failed to parse '%s' as a regex", m.Name)
			}
		}
	}
	return nil
}
