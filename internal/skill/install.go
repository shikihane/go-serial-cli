package skill

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed assets/serial-cli
var bundledSkillFS embed.FS

const bundledSkillRoot = "assets/serial-cli"

type InstallOptions struct {
	Source  string
	To      string
	HomeDir string
}

type InstallResult struct {
	Installed []string
}

func Install(opts InstallOptions) (InstallResult, error) {
	targets, err := resolveTargets(opts)
	if err != nil {
		return InstallResult{}, err
	}

	if opts.Source == "" {
		return installBundled(targets)
	}

	source, err := filepath.Abs(opts.Source)
	if err != nil {
		return InstallResult{}, err
	}
	if err := validateSource(source); err != nil {
		return InstallResult{}, err
	}

	return installDirectory(source, targets)
}

func installBundled(targets []string) (InstallResult, error) {
	name := bundledSkillName()
	var installed []string
	for _, target := range targets {
		dst := filepath.Join(target, name)
		if err := copyBundledSkill(dst); err != nil {
			return InstallResult{}, err
		}
		installed = append(installed, dst)
	}
	return InstallResult{Installed: installed}, nil
}

func installDirectory(source string, targets []string) (InstallResult, error) {
	name := skillName(source)
	var installed []string
	for _, target := range targets {
		dst := filepath.Join(target, name)
		if err := copyDir(source, dst); err != nil {
			return InstallResult{}, err
		}
		installed = append(installed, dst)
	}
	return InstallResult{Installed: installed}, nil
}

func validateSource(source string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("skill source must be a directory: %s", source)
	}
	if _, err := os.Stat(filepath.Join(source, "SKILL.md")); err != nil {
		return fmt.Errorf("skill source must contain SKILL.md: %w", err)
	}
	return nil
}

func resolveTargets(opts InstallOptions) ([]string, error) {
	if opts.To != "" {
		target, err := resolveTarget(opts.To, opts.HomeDir)
		if err != nil {
			return nil, err
		}
		return []string{target}, nil
	}

	home, err := homeDir(opts.HomeDir)
	if err != nil {
		return nil, err
	}
	return []string{
		filepath.Join(home, ".codex", "skills"),
		filepath.Join(home, ".claude", "skills"),
	}, nil
}

func resolveTarget(to string, explicitHome string) (string, error) {
	switch strings.ToLower(to) {
	case "codex":
		home, err := homeDir(explicitHome)
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".codex", "skills"), nil
	case "claude":
		home, err := homeDir(explicitHome)
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".claude", "skills"), nil
	default:
		return expandHome(to, explicitHome)
	}
}

func homeDir(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot find home directory")
	}
	return home, nil
}

func expandHome(path string, explicitHome string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := homeDir(explicitHome)
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func skillName(source string) string {
	if name := skillNameFromJSON(filepath.Join(source, "skill.json")); name != "" {
		return sanitizeName(name)
	}
	if name := skillNameFromMarkdown(filepath.Join(source, "SKILL.md")); name != "" {
		return sanitizeName(name)
	}
	base := filepath.Base(source)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "serial-cli"
	}
	if base == "go_serial_cli" || base == "go-serial-cli" {
		return "serial-cli"
	}
	return sanitizeName(base)
}

func skillNameFromJSON(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var metadata struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return ""
	}
	return metadata.Name
}

func skillNameFromMarkdown(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return skillNameFromMarkdownBytes(data)
}

func skillNameFromMarkdownBytes(data []byte) string {
	lines := strings.Split(string(data), "\n")
	if name := skillNameFromFrontmatter(lines); name != "" {
		return name
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

func skillNameFromFrontmatter(lines []string) string {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "---" {
			return ""
		}
		if strings.HasPrefix(line, "name:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "name:")), `"'`)
		}
	}
	return ""
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	replacer := strings.NewReplacer(" ", "-", "_", "-")
	name = replacer.Replace(name)
	if name == "" {
		return "serial-cli"
	}
	return name
}

func bundledSkillName() string {
	data, err := bundledSkillFS.ReadFile(filepath.ToSlash(filepath.Join(bundledSkillRoot, "SKILL.md")))
	if err != nil {
		return "serial-cli"
	}
	if name := skillNameFromMarkdownBytes(data); name != "" {
		return sanitizeName(name)
	}
	return "serial-cli"
}

func copyDir(src string, dst string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		pathAbs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if isWithin(pathAbs, dstAbs) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(srcAbs, pathAbs)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if shouldSkip(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyBundledSkill(dst string) error {
	return fs.WalkDir(bundledSkillFS, bundledSkillRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(bundledSkillRoot, filepath.FromSlash(path))
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := bundledSkillFS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func isWithin(path string, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func shouldSkip(rel string, d os.DirEntry) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case ".git", ".codegraphcontext", "bin", "dist":
		return true
	}
	if runtime.GOOS == "windows" && strings.EqualFold(parts[0], "Thumbs.db") {
		return true
	}
	return false
}

func copyFile(src string, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
