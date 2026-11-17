package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
)

// launchctl print separates configured, inherited and default environment.
// Compare the job's configured values and home, not unrelated system defaults.
type launchRegistration struct {
	args []string
	env  map[string]string
	home string
}

func launchBlock(s, key string) ([]string, bool) {
	var lines []string
	active := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !active {
			if line == key+" = {" {
				active = true
			}
			continue
		}
		if line == "}" {
			return lines, true
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return nil, false
}

func loadedRegistration(s string, j launchdJob, userHome string) (launchRegistration, error) {
	args, ok := launchBlock(s, "arguments")
	if !ok {
		return launchRegistration{}, fmt.Errorf("loaded LaunchAgent arguments unavailable")
	}
	expected := j.ProgramArguments
	if !reflect.DeepEqual(args, expected) {
		return launchRegistration{}, fmt.Errorf("loaded LaunchAgent arguments differ from its plist")
	}
	effective := map[string]string{}
	configured := map[string]string{}
	for _, block := range []string{"default environment", "inherited environment", "environment"} {
		lines, _ := launchBlock(s, block)
		for _, line := range lines {
			k, v, ok := strings.Cut(line, " => ")
			if !ok {
				return launchRegistration{}, fmt.Errorf("loaded LaunchAgent environment is unreadable")
			}
			effective[k] = v
			if block == "environment" && k != "OSLogRateLimit" && k != "XPC_SERVICE_NAME" {
				configured[k] = v
			}
		}
	}
	if !reflect.DeepEqual(configured, nonNilEnvironment(j.EnvironmentVariables)) {
		return launchRegistration{}, fmt.Errorf("loaded LaunchAgent environment differs from its plist")
	}
	for k, v := range j.EnvironmentVariables {
		if effective[k] != v {
			return launchRegistration{}, fmt.Errorf("loaded LaunchAgent environment differs from its plist")
		}
	}
	base := effective["HOME"]
	if base == "" {
		base = userHome
	}
	home := effective["ZM_HOME"]
	if home == "" {
		home = filepath.Join(base, ".zm")
	}
	expectedHome := j.EnvironmentVariables["ZM_HOME"]
	if expectedHome == "" {
		expectedBase := j.EnvironmentVariables["HOME"]
		if expectedBase == "" {
			expectedBase = userHome
		}
		expectedHome = filepath.Join(expectedBase, ".zm")
	}
	if home != expectedHome {
		return launchRegistration{}, fmt.Errorf("loaded LaunchAgent home differs from its plist")
	}
	return launchRegistration{args: args, env: configured, home: home}, nil
}

func nonNilEnvironment(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
