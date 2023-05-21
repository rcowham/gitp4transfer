package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const defaultConfig = `
import_depot:		import
import_path:		path
default_branch:		main
branch_mappings:
typemaps:
`

const map1Config = `
branch_mappings:
- source: main
- prefx:
`

func checkValue(t *testing.T, fieldname string, val string, expected string) {
	if val != expected {
		t.Fatalf("Error parsing %s, expected '%v' got '%v'", fieldname, expected, val)
	}
}

// func checkValueDuration(t *testing.T, fieldname string, val time.Duration, expected time.Duration) {
// 	if val != expected {
// 		t.Fatalf("Error parsing %s, expected %v got %v", fieldname, expected, val)
// 	}
// }

// func checkValueBool(t *testing.T, fieldname string, val bool, expected bool) {
// 	if val != expected {
// 		t.Fatalf("Error parsing %s, expected %v got %v", fieldname, expected, val)
// 	}
// }

func TestValidConfig(t *testing.T) {
	cfg := loadOrFail(t, defaultConfig)
	checkValue(t, "ImportDepot", cfg.ImportDepot, "import")
	checkValue(t, "ImportPath", cfg.ImportPath, "path")
	checkValue(t, "DefaultBranch", cfg.DefaultBranch, "main")
	assert.Empty(t, cfg.BranchMappings)
}

func TestEmptyConfig(t *testing.T) {
	cfg := loadOrFail(t, "")
	checkValue(t, "ImportDepot", cfg.ImportDepot, "import")
	checkValue(t, "ImportPath", cfg.ImportPath, "")
	checkValue(t, "DefaultBranch", cfg.DefaultBranch, "main")
	assert.Empty(t, cfg.BranchMappings)
}

func TestMap1(t *testing.T) {
	const config = `
branch_mappings:
- name: 	main
  prefix:
`
	cfg := loadOrFail(t, config)
	checkValue(t, "ImportDepot", cfg.ImportDepot, "import")
	checkValue(t, "ImportPath", cfg.ImportPath, "")
	checkValue(t, "DefaultBranch", cfg.DefaultBranch, "main")
	assert.Equal(t, 1, len(cfg.BranchMappings))
	assert.Equal(t, "main", cfg.BranchMappings[0].Name)
}

func TestMap2(t *testing.T) {
	const config = `
branch_mappings:
- name: 	main.*
  prefix:	fred
`
	cfg := loadOrFail(t, config)
	checkValue(t, "ImportDepot", cfg.ImportDepot, "import")
	checkValue(t, "ImportPath", cfg.ImportPath, "")
	checkValue(t, "DefaultBranch", cfg.DefaultBranch, "main")
	assert.Equal(t, 1, len(cfg.BranchMappings))
	assert.Equal(t, "main.*", cfg.BranchMappings[0].Name)
	assert.Equal(t, "fred", cfg.BranchMappings[0].Prefix)
}

func TestTypeMap1(t *testing.T) {
	const config = `
typemaps:
- text  //....txt
- binary  //....bin
`
	cfg := loadOrFail(t, config)
	checkValue(t, "ImportDepot", cfg.ImportDepot, "import")
	checkValue(t, "ImportPath", cfg.ImportPath, "")
	checkValue(t, "DefaultBranch", cfg.DefaultBranch, "main")
	assert.Equal(t, 0, len(cfg.BranchMappings))
	assert.Equal(t, 2, len(cfg.TypeMaps))
	assert.Equal(t, "text  //....txt", cfg.TypeMaps[0])
	assert.Equal(t, "binary  //....bin", cfg.TypeMaps[1])
	assert.True(t, cfg.ReTypeMaps[0].RePath.MatchString("//some/file.txt"))
	assert.True(t, cfg.ReTypeMaps[0].RePath.MatchString("//some/fredtxt"))
	assert.False(t, cfg.ReTypeMaps[0].RePath.MatchString("//some/fred.txt1"))
	assert.False(t, cfg.ReTypeMaps[0].RePath.MatchString("//some/fred.bin"))
	assert.True(t, cfg.ReTypeMaps[1].RePath.MatchString("//file.bin"))
	assert.True(t, cfg.ReTypeMaps[1].RePath.MatchString("//some/file.bin"))
}

func TestTypeMap2(t *testing.T) {
	const config = `
typemaps:
- text	//....txt
- binary	"//....bin"
`
	cfg := loadOrFail(t, config)
	checkValue(t, "ImportDepot", cfg.ImportDepot, "import")
	checkValue(t, "ImportPath", cfg.ImportPath, "")
	checkValue(t, "DefaultBranch", cfg.DefaultBranch, "main")
	assert.Equal(t, 0, len(cfg.BranchMappings))
	assert.Equal(t, 2, len(cfg.TypeMaps))
	assert.Equal(t, "text	//....txt", cfg.TypeMaps[0])
	assert.Equal(t, "binary	\"//....bin\"", cfg.TypeMaps[1])
}

func TestRegex(t *testing.T) {
	const config = `
branch_mappings:
- name: 	main.*[
  prefix:	fred
`
	_, err := Unmarshal([]byte(config))
	if err == nil {
		t.Fatalf("Expected regex error not seen")
	}
}

// func TestWrongValues(t *testing.T) {
// 	start := `log_path:			/p4/1/logs/log
// metrics_output:				/hxlogs/metrics/cmds.prom
// `
// 	ensureFail(t, start+`update_interval: 	'not duration'`, "duration")
// }

// func TestDefaultInterval(t *testing.T) {
// 	cfg := loadOrFail(t, `
// log_path:			/p4/1/logs/log
// metrics_output:		/hxlogs/metrics/cmds.prom
// server_id:			myserverid
// sdp_instance: 		1
// `)
// 	if cfg.UpdateInterval != 15*time.Second {
// 		t.Errorf("Failed default interval: %v", cfg.UpdateInterval)
// 	}
// 	if !cfg.OutputCmdsByUser {
// 		t.Errorf("Failed default output_cmds_by_user")
// 	}
// 	if runtime.GOOS == "windows" {
// 		if cfg.CaseSensitiveServer {
// 			t.Errorf("Failed default case_sensitive_server on Windows")
// 		}
// 	} else {
// 		if !cfg.CaseSensitiveServer {
// 			t.Errorf("Failed default case_sensitive_server on Linux/Mac")
// 		}
// 	}
// }

// func TestRegex(t *testing.T) {
// 	// Invalid regex should cause error
// 	cfgString := `
// log_path:					/p4/1/logs/log
// metrics_output:				/hxlogs/metrics/cmds.prom
// server_id:					myserverid
// output_cmds_by_user_regex: 	"[.*"
// `
// 	_, err := Unmarshal([]byte(cfgString))
// 	if err == nil {
// 		t.Fatalf("Expected regex error not seen")
// 	}
// }

func ensureFail(t *testing.T, cfgString string, desc string) {
	_, err := Unmarshal([]byte(cfgString))
	if err == nil {
		t.Fatalf("Expected config err not found: %s", desc)
	}
	t.Logf("Config err: %v", err.Error())
}

func loadOrFail(t *testing.T, cfgString string) *Config {
	cfg, err := Unmarshal([]byte(cfgString))
	if err != nil {
		t.Fatalf("Failed to read config: %v", err.Error())
	}
	return cfg
}
