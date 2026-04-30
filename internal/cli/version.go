package cli

import (
	"runtime"
	"runtime/debug"
)

var (
	BuildVersion = "dev"
	BuildCommit  = "unknown"
	BuildBuiltAt = "unknown"
)

type VersionInfo struct {
	Version   string
	Commit    string
	BuiltAt   string
	GoVersion string
}

func (v VersionInfo) IsZero() bool {
	return v.Version == "" && v.Commit == "" && v.BuiltAt == "" && v.GoVersion == ""
}

func (v VersionInfo) Normalize() VersionInfo {
	if v.Version == "" {
		v.Version = "dev"
	}
	if v.Commit == "" {
		v.Commit = "unknown"
	}
	if v.BuiltAt == "" {
		v.BuiltAt = "unknown"
	}
	if v.GoVersion == "" {
		v.GoVersion = runtime.Version()
	}
	return v
}

func DefaultVersionInfo() VersionInfo {
	info := VersionInfo{
		Version:   BuildVersion,
		Commit:    BuildCommit,
		BuiltAt:   BuildBuiltAt,
		GoVersion: runtime.Version(),
	}.Normalize()
	if info.Commit == "unknown" {
		if buildInfo, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range buildInfo.Settings {
				if setting.Key == "vcs.revision" && setting.Value != "" {
					info.Commit = setting.Value
					break
				}
			}
		}
	}
	return info
}
