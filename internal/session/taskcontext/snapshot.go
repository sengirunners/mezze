// Package taskcontext creates a bounded, frozen native Claude skills plugin.
// It never loads repository settings or existing plugin component manifests.
package taskcontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const Namespace = "zm-task-repo"
const MaxSkills = 32
const MaxFiles = 1024
const MaxFileBytes = 1 << 20
const MaxBytes = 8 << 20
const MaxDepth = 8

type Result struct {
	Plugin      string
	Directories []string
	Skills      []string
}
type entry struct {
	Path, SHA256 string
	Bytes        int
}
type copier struct {
	ctx          context.Context
	files, bytes int
	entries      []entry
}

// Build reads only .claude/skills from repository-root-to-cwd ancestors.
// No repository: only cwd. Errors never leave a partial plugin behind.
// The caller owns scratch's lifetime, including holder adoption after restart.
func Build(ctx context.Context, cwd, scratch string) (result Result, err error) {
	dirs, err := directories(cwd)
	if err != nil {
		return result, err
	}
	result.Directories = dirs
	dest := filepath.Join(scratch, "task-context")
	if err = os.Mkdir(dest, 0700); err != nil {
		return result, fmt.Errorf("task context directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dest)
		}
	}()
	plugin := filepath.Join(dest, "plugin")
	if err = os.MkdirAll(filepath.Join(plugin, "skills"), 0700); err != nil {
		return result, err
	}
	c := copier{ctx: ctx}
	seen := map[string]bool{}
	for _, dir := range dirs {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		root, e := os.OpenRoot(dir)
		if e != nil {
			return result, e
		}
		e = func() error {
			defer root.Close()
			skills, e := openDirs(root, ".claude", "skills")
			if errors.Is(e, os.ErrNotExist) {
				return nil
			}
			if e != nil {
				return e
			}
			defer skills.Close()
			before, e := skills.Stat(".")
			if e != nil {
				return e
			}
			names, e := children(skills)
			if e != nil {
				return e
			}
			for _, name := range names {
				if !validName.MatchString(name) {
					return fmt.Errorf("unsupported skill directory %q", name)
				}
				if seen[strings.ToLower(name)] {
					return fmt.Errorf("ambiguous skill name %q in task ancestors", name)
				}
				if len(seen) >= MaxSkills {
					return errors.New("task skills exceed 32 skills")
				}
				seen[strings.ToLower(name)] = true
				skill, e := openDirs(skills, name)
				if e != nil {
					return e
				}
				target := filepath.Join(plugin, "skills", name)
				e = func() error {
					defer skill.Close()
					b, e := readRegular(skill, "SKILL.md", MaxFileBytes)
					if e != nil {
						return fmt.Errorf("skill %q needs a regular SKILL.md: %w", name, e)
					}
					if e = validateName(b, name); e != nil {
						return fmt.Errorf("skill %q: %w", name, e)
					}
					return c.copy(skill, target, "skills/"+name, 0)
				}()
				if e != nil {
					return e
				}
				result.Skills = append(result.Skills, name)
			}
			return unchangedDirectory(skills, before)
		}()
		if e != nil {
			return result, e
		}
	}
	if len(result.Skills) > 0 {
		if err = os.Mkdir(filepath.Join(plugin, ".claude-plugin"), 0700); err != nil {
			return result, err
		}
		b, _ := json.Marshal(map[string]string{"name": Namespace, "version": "0.0.0"})
		if err = os.WriteFile(filepath.Join(plugin, ".claude-plugin/plugin.json"), b, 0600); err != nil {
			return result, err
		}
		result.Plugin = plugin
	} else {
		if err = os.RemoveAll(plugin); err != nil {
			return result, err
		}
	}
	audit, _ := json.MarshalIndent(struct {
		Namespace           string
		Directories, Skills []string
		Files               []entry
	}{Namespace, dirs, result.Skills, c.entries}, "", "  ")
	err = os.WriteFile(filepath.Join(dest, "snapshot.json"), audit, 0600)
	return result, err
}

// All source operations remain capability-rooted. Reject symlinks outright,
// including contained links: do not silently change their resource semantics.
func openDirs(root *os.Root, names ...string) (*os.Root, error) {
	current := root
	for i, name := range names {
		before, err := current.Lstat(name)
		if err != nil {
			if i > 0 {
				current.Close()
			}
			return nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			if i > 0 {
				current.Close()
			}
			return nil, errors.New("task skill path must be a real directory")
		}
		next, err := current.OpenRoot(name)
		if i > 0 {
			current.Close()
		}
		if err != nil {
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			return nil, errors.New("task skill directory changed during snapshot")
		}
		current = next
	}
	return current, nil
}
func children(root *os.Root) ([]string, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(MaxFiles + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > MaxFiles {
		return nil, errors.New("task skill directory has too many entries")
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}
func readRegular(root *os.Root, name string, limit int) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > int64(limit) {
		return nil, errors.New("task skill resource is special or too large")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("task skill resource changed during snapshot")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || len(b) > limit || int64(len(b)) != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("task skill resource changed or exceeded its bound")
	}
	return b, nil
}
func (c *copier) copy(root *os.Root, dest, rel string, depth int) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if depth > MaxDepth {
		return errors.New("task skill resource nesting exceeds 8")
	}
	if err := os.Mkdir(dest, 0700); err != nil {
		return err
	}
	before, err := root.Stat(".")
	if err != nil {
		return err
	}
	names, err := children(root)
	if err != nil {
		return err
	}
	for _, name := range names {
		c.files++
		if c.files > MaxFiles {
			return errors.New("task skills exceed 1024 resource entries")
		}
		if err = c.ctx.Err(); err != nil {
			return err
		}
		switch strings.ToLower(name) {
		case ".claude", ".claude-plugin", ".mcp.json", ".lsp.json", "settings.json", "settings.local.json", "hooks":
			return fmt.Errorf("task skill contains unsupported configuration component %q", name)
		}
		info, e := root.Lstat(name)
		if e != nil {
			return e
		}
		if info.IsDir() {
			sub, e := openDirs(root, name)
			if e != nil {
				return e
			}
			e = c.copy(sub, filepath.Join(dest, name), rel+"/"+name, depth+1)
			sub.Close()
			if e != nil {
				return e
			}
			continue
		}
		b, e := readRegular(root, name, MaxFileBytes)
		if e != nil {
			return e
		}
		if depth == 0 && name == "SKILL.md" {
			if e = validateName(b, filepath.Base(rel)); e != nil {
				return e
			}
		}
		c.bytes += len(b)
		if c.bytes > MaxBytes {
			return errors.New("task skills exceed 8 MiB")
		}
		mode := os.FileMode(0600)
		if info.Mode()&0111 != 0 {
			mode = 0700
		}
		if e = os.WriteFile(filepath.Join(dest, name), b, mode); e != nil {
			return e
		}
		sum := sha256.Sum256(b)
		c.entries = append(c.entries, entry{rel + "/" + name, hex.EncodeToString(sum[:]), len(b)})
	}
	return unchangedDirectory(root, before)
}
func unchangedDirectory(root *os.Root, before os.FileInfo) error {
	after, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("task skill directory changed during snapshot")
	}
	return nil
}

var validKey = regexp.MustCompile("^[a-z][a-z-]*$")
var validName = regexp.MustCompile("^[a-z0-9][a-z0-9-]{0,63}$")

// Check identity without rewriting skill content. Fail closed on YAML forms
// that can hide/merge/override a command name; Claude parses the preserved file.
func validateName(b []byte, dir string) error {
	if !utf8.Valid(b) {
		return errors.New("SKILL.md is not UTF-8")
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return errors.New("SKILL.md requires explicit YAML frontmatter")
	}
	seen := map[string]bool{}
	block := false
	blockContent := false
	blockIndent := 0
	leadingBlankIndent := 0
	for _, line := range lines[1:] {
		if unsafeScalarCharacters(line) {
			return errors.New("control characters and Unicode line separators are unsupported in skill frontmatter")
		}
		if line == "---" {
			if block && !blockContent {
				return errors.New("empty skill frontmatter block")
			}
			if !seen["description"] {
				return errors.New("skill requires an explicit description")
			}
			return nil
		}
		if strings.Trim(line, " ") == "" {
			if block && !blockContent && len(line) > leadingBlankIndent {
				leadingBlankIndent = len(line)
			}
			continue
		}
		if !block && strings.HasPrefix(strings.Trim(line, " "), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !block || line[0] == '\t' {
				return errors.New("nested YAML skill frontmatter is unsupported; use scalar fields")
			}
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if !blockContent && leadingBlankIndent > indent {
				return errors.New("leading empty skill block lines exceed its content indentation")
			}
			if blockIndent == 0 {
				blockIndent = indent
			} else if indent < blockIndent {
				return errors.New("inconsistent skill frontmatter block indentation")
			}
			blockContent = true
			continue
		}
		if block && !blockContent {
			return errors.New("empty skill frontmatter block")
		}
		block = false
		key, value, ok := strings.Cut(line, ":")
		if !ok || !validKey.MatchString(key) || seen[key] || value != "" && value[0] != ' ' {
			return errors.New("unsupported or duplicate skill frontmatter key")
		}
		seen[key] = true
		switch key {
		case "name", "description", "argument-hint", "disable-model-invocation", "user-invocable", "allowed-tools", "disallowed-tools", "paths", "model", "context", "agent", "shell", "license", "compatibility":
		default:
			return fmt.Errorf("unsupported task skill frontmatter %q", key)
		}
		value = strings.Trim(value, " ")
		if (key == "description" || key == "argument-hint") && (value == "|" || value == ">" || value == "|-" || value == ">-") {
			block = true
			blockContent = false
			blockIndent = 0
			leadingBlankIndent = 0
			continue
		}
		scalar, err := scalarValue(value)
		if err != nil {
			return err
		}
		if key != "disable-model-invocation" && key != "user-invocable" && value[0] != '"' && value[0] != '\'' {
			if yamlBooleanText(scalar) || scalar[0] >= '0' && scalar[0] <= '9' || strings.ContainsAny(scalar[:1], "+-.") {
				return errors.New("quote boolean or numeric-looking skill text")
			}
		}
		if key == "agent" && scalar != "general-purpose" && scalar != "Explore" && scalar != "Plan" {
			return errors.New("task skills do not load custom repository agents")
		}
		if key == "context" && scalar != "fork" {
			return errors.New("unsupported skill context")
		}
		if key == "name" && scalar != dir {
			return errors.New("skill name must match its directory")
		}
		if key == "disable-model-invocation" || key == "user-invocable" {
			if value != "true" && value != "false" {
				return errors.New("skill invocation flags must be booleans")
			}
		}
	}
	return errors.New("unterminated skill frontmatter")
}
func scalarValue(value string) (string, error) {
	if value == "" {
		return "", errors.New("empty skill frontmatter value")
	}
	if value[0] == '"' {
		var text string
		if json.Unmarshal([]byte(value), &text) != nil || text == "" || unsafeScalarCharacters(text) {
			return "", errors.New("unsupported quoted skill frontmatter")
		}
		return text, nil
	}
	if value[0] == '\'' {
		if len(value) <= 2 || value[len(value)-1] != '\'' {
			return "", errors.New("unterminated skill frontmatter string")
		}
		inside := value[1 : len(value)-1]
		for i := 0; i < len(inside); i++ {
			if inside[i] == '\'' {
				if i+1 >= len(inside) || inside[i+1] != '\'' {
					return "", errors.New("ambiguous skill frontmatter string")
				}
				i++
			}
		}
		return strings.ReplaceAll(inside, "''", "'"), nil
	}
	if (strings.ContainsAny(value[:1], "[]{}&*!|>#%,@`?:") || unsafeScalarCharacters(value)) || strings.Contains(value, ": ") || strings.Contains(value, " #") || strings.HasSuffix(value, ":") || strings.EqualFold(value, "null") || value == "~" {
		return "", errors.New("task skill frontmatter supports plain or quoted scalars, not YAML indicators, implicit nulls or ambiguous mappings")
	}
	return value, nil
}

// This is deliberately stricter than YAML's printable set. Tabs and alternate
// line separators would make our line/scalar boundary differ from the native
// loader. Check decoded double-quoted strings as well as literal header text.
func unsafeScalarCharacters(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || r == '\ufeff' || r == '\ufffe' || r == '\uffff' {
			return true
		}
	}
	return false
}

// Quote all common YAML 1.1/1.2 boolean spellings in string-valued fields;
// invocation flags separately require exactly the literal true or false.
func yamlBooleanText(value string) bool {
	switch strings.ToLower(value) {
	case "true", "false", "yes", "no", "on", "off", "y", "n":
		return true
	}
	return false
}

func directories(cwd string) ([]string, error) {
	cwd = filepath.Clean(cwd)
	if !filepath.IsAbs(cwd) {
		return nil, errors.New("task cwd must be absolute")
	}
	chain := []string{}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if len(chain) >= 32 {
			return nil, errors.New("task repository ancestry exceeds 32 directories")
		}
		chain = append(chain, dir)
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
				chain[i], chain[j] = chain[j], chain[i]
			}
			return chain, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return []string{cwd}, nil
}
