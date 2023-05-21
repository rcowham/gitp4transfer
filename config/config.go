package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/rcowham/gitp4transfer/journal"
	yaml "gopkg.in/yaml.v2"
)

const DefaultDepot = "import"
const DefaultBranch = "main"

type BranchMapping struct {
	Name   string `yaml:"name"`   // Regex for branch
	Prefix string `yaml:"prefix"` // Prefix to prepend to matching branches
}

// ReTypeMap - parsed into regexp
type RegexpTypeMap struct {
	Filetype journal.FileType // String for path
	RePath   *regexp.Regexp   // Compiled regexp
}

// Config for gitp4transfer
type Config struct {
	ImportDepot    string          `yaml:"import_depot"`
	ImportPath     string          `yaml:"import_path"`
	DefaultBranch  string          `yaml:"default_branch"`
	BranchMappings []BranchMapping `yaml:"branch_mappings"`
	TypeMaps       []string        `yaml:"typemaps"`
	ReTypeMaps     []RegexpTypeMap
}

// Unmarshal the config
func Unmarshal(config []byte) (*Config, error) {
	// Default values specified here
	cfg := &Config{
		ImportDepot:   "import",
		DefaultBranch: "main",
		ReTypeMaps:    make([]RegexpTypeMap, 0),
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
	if len(c.TypeMaps) > 0 {
		for _, m := range c.TypeMaps {
			parts := strings.Fields(m)
			if len(parts) != 2 {
				return fmt.Errorf("failed to split '%s' on a space", m)
			}
			ftype := parts[0]
			reStr := parts[1]
			if !strings.Contains(ftype, "binary") && !strings.Contains(ftype, "text") {
				return fmt.Errorf("typemaps must contain either 'binary' or 'text' in first part: %s", m)
			}
			reStr = strings.ReplaceAll(reStr, "...", ".*")
			reStr += "$"
			if rePath, err := regexp.Compile(reStr); err != nil {
				return fmt.Errorf("failed to parse '%s' as a regex", reStr)
			} else {
				baseType := journal.CText
				if strings.Contains(ftype, "binary") {
					baseType = journal.Binary // Compressed or not handled later
				}
				c.ReTypeMaps = append(c.ReTypeMaps, RegexpTypeMap{Filetype: baseType, RePath: rePath})
			}
		}
	}
	return nil
}
