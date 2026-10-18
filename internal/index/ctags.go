package index

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/laughingmandev/loa/internal/utils"
)

type Symbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line,omitempty"`
	Signature string `json:"signature,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Language  string `json:"language,omitempty"`
}

type Index struct {
	mu        sync.RWMutex
	root      string
	binary    string
	available bool
	warning   string
	files     map[string][]Symbol
}

func New(root string) *Index {
	return &Index{
		root:  root,
		files: map[string][]Symbol{},
	}
}

func (i *Index) Available() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()

	return i.available
}

func (i *Index) Warning() string {
	i.mu.RLock()
	defer i.mu.RUnlock()

	return i.warning
}

func (i *Index) Probe(ctx context.Context) error {
	candidates := []string{
		"ctags",
		"ctags-universal",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".loa", "bin", "ctags"))
	}

	seen := map[string]bool{}
	var problems []string
	foundAny := false

	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}

		foundAny = true

		// Avoid probing the same executable twice if, for example,
		// /usr/bin/ctags points to /usr/bin/ctags-universal.
		realPath := path

		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			realPath = resolved
		}

		if seen[realPath] {
			continue
		}

		seen[realPath] = true

		cmd := exec.CommandContext(
			ctx,
			path,
			"--list-features",
		)

		out, err := cmd.Output()
		if err != nil {
			problems = append(
				problems,
				fmt.Sprintf(
					"%s feature probe failed: %v",
					candidate,
					err,
				),
			)
			continue
		}

		if !containsFeature(string(out), "json") {
			problems = append(
				problems,
				fmt.Sprintf(
					"%s lacks JSON support",
					candidate,
				),
			)
			continue
		}

		i.mu.Lock()
		i.binary = path
		i.available = true
		i.warning = ""
		i.mu.Unlock()

		return nil
	}

	var err error

	if !foundAny {
		err = errors.New("Universal Ctags not found")
	} else if len(problems) > 0 {
		err = errors.New(strings.Join(problems, "; "))
	} else {
		err = errors.New("no usable Universal Ctags executable found")
	}

	i.setUnavailable(
		err.Error() + "; symbol index disabled",
	)

	return err
}

func containsFeature(s, wanted string) bool {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)

		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}

		if fields[0] == wanted {
			return true
		}
	}

	return false
}

func (i *Index) setUnavailable(w string) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.binary = ""
	i.available = false
	i.warning = w
	i.files = map[string][]Symbol{}
}

func (i *Index) setWarning(w string) {
	i.mu.Lock()
	i.warning = w
	i.mu.Unlock()
}

func (i *Index) binaryPath() string {
	i.mu.RLock()
	defer i.mu.RUnlock()

	return i.binary
}

func (i *Index) Rebuild(ctx context.Context) error {
	if !i.Available() || i.binaryPath() == "" {
		if err := i.Probe(ctx); err != nil {
			return err
		}
	}

	syms, runErr := i.run(
		ctx,
		"-R",
		i.root,
	)

	_, isLoaIgnored := utils.ReadLoaignore(i.root)
	files := groupAndNormalize(filterExcludedSymbols(syms, isLoaIgnored))

	_ = filepath.WalkDir(i.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(i.root, path)
		if err != nil {
			return nil
		}
		relClean := filepath.ToSlash(filepath.Clean(rel))
		if relClean == "." {
			return nil
		}
		if d.IsDir() {
			if isExcludedPath(relClean, isLoaIgnored) {
				return filepath.SkipDir
			}
			return nil
		}
		if isExcludedPath(relClean, isLoaIgnored) {
			return nil
		}
		if _, exists := files[relClean]; !exists {
			if isTextFile(path) {
				files[relClean] = []Symbol{}
			}
		}
		return nil
	})

	if runErr != nil && len(files) == 0 {
		return runErr
	}

	i.mu.Lock()
	i.files = files
	i.available = true

	if runErr != nil {
		i.warning = "ctags reported errors; partial index kept: " + runErr.Error()
	} else {
		i.warning = ""
	}

	i.mu.Unlock()

	return nil
}

func (i *Index) RefreshFile(ctx context.Context, rel string) error {
	if !i.Available() || i.binaryPath() == "" {
		return errors.New("symbol index unavailable")
	}

	relClean := filepath.ToSlash(filepath.Clean(rel))
	_, isLoaIgnored := utils.ReadLoaignore(i.root)
	if isExcludedPath(relClean, isLoaIgnored) {
		i.RemoveFile(relClean)
		return nil
	}

	abs, err := filepath.Abs(filepath.Join(i.root, rel))
	if err != nil {
		return err
	}

	if _, statErr := os.Stat(abs); statErr != nil {
		i.RemoveFile(relClean)
		return nil
	}

	syms, runErr := i.run(ctx, abs)

	if runErr != nil {
		i.RemoveFile(relClean)

		i.setWarning(
			"ctags refresh failed for " +
				relClean +
				"; stale entry removed: " +
				runErr.Error(),
		)

		return runErr
	}

	for n := range syms {
		if r, e := filepath.Rel(i.root, syms[n].File); e == nil {
			syms[n].File = filepath.ToSlash(r)
		}
	}

	norm := groupAndNormalize(filterExcludedSymbols(syms, isLoaIgnored))[relClean]

	i.mu.Lock()

	if len(norm) == 0 {
		if isTextFile(abs) {
			i.files[relClean] = []Symbol{}
		} else {
			delete(i.files, relClean)
		}
	} else {
		i.files[relClean] = norm
	}

	i.warning = ""
	i.mu.Unlock()

	return nil
}

var builtinExcludes = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".venv":        true,
	"venv":         true,
	"target":       true,
	".cache":       true,
	"coverage":     true,
	".loa":         true,
	".loa.json":    true,
	".loaignore":   true,
	"loa":          true,
	"loa-sandbox":  true,
}

func isIgnoredPath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	base := filepath.Base(clean)
	if base == ".loa.json" {
		return true
	}
	return clean == ".loa" || strings.HasPrefix(clean, ".loa/") || strings.Contains(clean, "/.loa/")
}

func isExcludedPath(rel string, isLoaIgnored func(string) bool) bool {
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || clean == ".." {
		return true
	}
	if isIgnoredPath(clean) {
		return true
	}
	base := filepath.Base(clean)
	if strings.Contains(base, ".loa-write-") {
		return true
	}
	parts := strings.Split(clean, "/")
	prefix := ""
	for idx, p := range parts {
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, ".") || builtinExcludes[p] {
			return true
		}
		if idx == 0 {
			prefix = p
		} else {
			prefix = prefix + "/" + p
		}
		if isLoaIgnored != nil && (isLoaIgnored(p) || isLoaIgnored(prefix)) {
			return true
		}
	}
	if isLoaIgnored != nil && isLoaIgnored(clean) {
		return true
	}
	return false
}

func isTextFile(absPath string) bool {
	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(absPath)
	if err != nil {
		return false
	}
	defer f.Close()

	var buf [8192]byte
	n, err := f.Read(buf[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	sample := buf[:n]
	if bytes.IndexByte(sample, 0) != -1 {
		return false
	}
	if n == len(buf) {
		for len(sample) > 0 && !utf8.Valid(sample) && len(buf)-len(sample) < utf8.UTFMax {
			sample = sample[:len(sample)-1]
		}
	}
	return utf8.Valid(sample)
}

func filterIgnoredSymbols(syms []Symbol) []Symbol {
	return filterExcludedSymbols(syms, nil)
}

func filterExcludedSymbols(syms []Symbol, isLoaIgnored func(string) bool) []Symbol {
	if len(syms) == 0 {
		return nil
	}

	out := make([]Symbol, 0, len(syms))
	for _, s := range syms {
		if isExcludedPath(s.File, isLoaIgnored) {
			continue
		}
		out = append(out, s)
	}

	return out
}

func (i *Index) RemoveFile(rel string) {
	i.mu.Lock()
	defer i.mu.Unlock()

	delete(i.files, filepath.ToSlash(rel))
}

func (i *Index) SymbolsForFile(rel string) []Symbol {
	i.mu.RLock()
	defer i.mu.RUnlock()

	return append(
		[]Symbol(nil),
		i.files[filepath.ToSlash(rel)]...,
	)
}

func (i *Index) Files() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()

	var out []string
	for k := range i.files {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (i *Index) Find(query string, limit int) []Symbol {
	q := strings.ToLower(strings.TrimSpace(query))

	if q == "" {
		return nil
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	var exact []Symbol
	var prefix []Symbol
	var contains []Symbol

	for _, ss := range i.files {
		for _, s := range ss {
			n := strings.ToLower(s.Name)

			switch {
			case n == q:
				exact = append(exact, s)

			case strings.HasPrefix(n, q):
				prefix = append(prefix, s)

			case strings.Contains(n, q):
				contains = append(contains, s)
			}
		}
	}

	out := append(exact, prefix...)
	out = append(out, contains...)

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}

	return out
}

func (i *Index) Snapshot() map[string][]Symbol {
	i.mu.RLock()
	defer i.mu.RUnlock()

	out := make(map[string][]Symbol, len(i.files))

	for k, v := range i.files {
		out[k] = append([]Symbol(nil), v...)
	}

	return out
}

func (i *Index) run(ctx context.Context, targets ...string) ([]Symbol, error) {
	binary := i.binaryPath()

	if binary == "" {
		return nil, errors.New("ctags executable not selected")
	}

	args := []string{
		"--output-format=json",
		"--fields=+nKeSlZ",
		"--extras=-F",

		"--exclude=.git",
		"--exclude=node_modules",
		"--exclude=vendor",
		"--exclude=dist",
		"--exclude=build",
		"--exclude=.venv",
		"--exclude=venv",
		"--exclude=target",
		"--exclude=.cache",
		"--exclude=coverage",
		"--exclude=.loa",
		"--exclude=.loa.json",
		"--exclude=loa",
		"--exclude=loa-sandbox",

		"-o",
		"-",
	}

	ctagsExcludes, _ := utils.ReadLoaignore(i.root)
	for _, excl := range ctagsExcludes {
		args = append(args, "--exclude="+excl)
	}

	args = append(args, targets...)

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = i.root

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	var syms []Symbol

	sc := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for sc.Scan() {
		var row map[string]any

		if json.Unmarshal(sc.Bytes(), &row) != nil {
			continue
		}

		if row["_type"] != "tag" {
			continue
		}

		s := symbolFrom(row)

		if s.Name != "" && s.File != "" {
			syms = append(syms, s)
		}
	}

	if err := sc.Err(); err != nil {
		return nil, err
	}

	for n := range syms {
		if filepath.IsAbs(syms[n].File) {
			if rel, err := filepath.Rel(i.root, syms[n].File); err == nil {
				syms[n].File = filepath.ToSlash(rel)
			}
		} else {
			syms[n].File = filepath.ToSlash(syms[n].File)
		}
	}

	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())

		if msg == "" {
			msg = runErr.Error()
		}

		return syms, fmt.Errorf("ctags: %s", msg)
	}

	return syms, nil
}

func symbolFrom(m map[string]any) Symbol {
	file := str(m["path"])

	if file == "" {
		file = str(m["input"])
	}

	return Symbol{
		Name:      str(m["name"]),
		Kind:      str(m["kind"]),
		File:      file,
		Line:      num(m["line"]),
		EndLine:   num(m["end"]),
		Signature: str(m["signature"]),
		Scope:     str(m["scope"]),
		Language:  str(m["language"]),
	}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
}

func num(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)

	case string:
		n, _ := strconv.Atoi(x)
		return n
	}

	return 0
}

func groupAndNormalize(syms []Symbol) map[string][]Symbol {
	m := map[string][]Symbol{}

	for _, s := range syms {
		m[s.File] = append(m[s.File], s)
	}

	for file, ss := range m {
		sort.SliceStable(
			ss,
			func(a, b int) bool {
				return ss[a].Line < ss[b].Line
			},
		)

		for n := range ss {
			if ss[n].EndLine > ss[n].Line {
				continue
			}

			if n+1 < len(ss) && ss[n+1].Line > ss[n].Line {
				ss[n].EndLine = ss[n+1].Line - 1
			} else {
				ss[n].EndLine = ss[n].Line + 80
			}
		}

		m[file] = ss
	}

	return m
}
