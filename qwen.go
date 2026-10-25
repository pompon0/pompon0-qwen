package main

import (
	"fmt"
	"regexp"
)

// QwenVersion selects a published npm version or dist-tag, never another package
// or a local/Git dependency. npm handles resolving tags and caching installations.
type QwenVersion string

var qwenVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

func (v *QwenVersion) Set(value string) error {
	if !qwenVersionPattern.MatchString(value) {
		return fmt.Errorf("invalid Qwen version %q: use an npm version or dist-tag", value)
	}
	*v = QwenVersion(value)
	return nil
}

func (v QwenVersion) String() string {
	if v == "" {
		return "latest"
	}
	return string(v)
}

func (QwenVersion) Type() string { return "version" }

func qwenCommand(version QwenVersion, arguments []string) []string {
	// Keep package discovery outside the project without changing Qwen's working
	// directory. npm installs missing packages in the sandbox home's npm cache.
	return append([]string{"/usr/bin/npm", "exec", "--yes", "--prefer-online",
		"--prefix=/home/qwen",
		"--package=@qwen-code/qwen-code@" + version.String(), "--", "qwen"}, arguments...)
}
