package tools

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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/index"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)


type Request struct {
	Kind        state.ToolKind  `json:"kind"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
}

type Manager struct {
	Root   string
	Index  *index.Index
	Config func() config.Config
	LLM    llm.Client
	GetTask func(uint64) (*state.TaskState, error)
	GetSessionID func() string
	mu     sync.RWMutex
	output map[uint64]string
}

func New(root string, idx *index.Index, cfg func() config.Config, lc llm.Client) *Manager {
	return &Manager{Root: root, Index: idx, Config: cfg, LLM: lc, output: map[uint64]string{}}
}

var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".pdf": true, ".zip": true, ".tar": true, ".gz": true, ".mp4": true,
	".bin": true, ".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".class": true, ".jar": true, ".war": true, ".ear": true,
}

func isBinaryExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return binaryExts[ext]
}

func isBinaryFile(path string) bool {
	if isBinaryExt(path) {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false
	}
	return bytes.IndexByte(buf[:n], 0) != -1
}

func sanitizeOutput(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\n' && c != '\r' && c != '\t') || c == 0x7F {
			b.WriteByte('?')
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func IsMutating(kind state.ToolKind) bool {
	switch kind {
	case state.ToolWriteFile, state.ToolPatchFile, state.ToolPatchASTNode, state.ToolCreateDir, state.ToolDeleteFile, state.ToolDeleteDir, state.ToolExecuteProcess, state.ToolExecuteShell:
		return true
	default:
		return false
	}
}

func IsDestructive(kind state.ToolKind) bool {
	return kind == state.ToolDeleteFile || kind == state.ToolDeleteDir
}

func (m *Manager) Execute(ctx context.Context, call state.ToolCall, activeTaskID uint64) state.ToolResult {
	start := time.Now()
	res := state.ToolResult{ToolCallID: call.ID, CreatedAt: start}
	req := Request{Kind: call.Kind}
	b, _ := json.Marshal(call.Input)
	req.Input = b
	out, exit, err := m.execute(ctx, req, call.ID, activeTaskID)
	out = sanitizeOutput(out)
	limit := m.Config().MaxStoredToolOutputBytes
	if limit <= 0 {
		limit = 4 << 20
	}
	if len(out) > limit && call.Kind != state.ToolAnalyzeLargeFile {
		out = out[:limit]
		res.Truncated = true
	}
	res.Output = out
	res.ExitCode = exit
	if err != nil {
		res.Error = err.Error()
		res.Success = false
	} else {
		res.Success = true
	}
	m.mu.Lock()
	m.output[call.ID] = out
	m.mu.Unlock()
	return res
}

func (m *Manager) execute(ctx context.Context, req Request, callID uint64, activeTaskID uint64) (string, *int, error) {
	switch req.Kind {
	case state.ToolReadFile:
		var x struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.readFile(x.Path)
	case state.ToolReadRange:
		var x struct {
			Path       string `json:"path"`
			Start, End int
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.readRange(x.Path, x.Start, x.End)
	case state.ToolSearchPath:
		var x struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.searchPath(x.Query, x.Limit)
	case state.ToolSearchText:
		var x struct {
			Pattern string `json:"pattern"`
			Regex   bool   `json:"regex"`
			Limit   int    `json:"limit"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.searchText(x.Pattern, x.Regex, x.Limit)
	case state.ToolFindSymbol:
		var x struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.findSymbol(x.Query, x.Limit)
	case state.ToolListSymbols:
		var x struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.listSymbols(x.Path)
	case state.ToolWriteFile:
		var x struct{ Path, Content string }
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.writeFile(ctx, x.Path, x.Content)
	case state.ToolPatchFile:
		var x struct {
			Path, Old, New string
			ReplaceAll     bool `json:"replace_all"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.patchFile(ctx, x.Path, x.Old, x.New, x.ReplaceAll)
	case state.ToolSearchTaskSteps:
		return m.searchTaskSteps(activeTaskID)
	case state.ToolReadTaskStep:
		var x struct {
			StepID uint64 `json:"step_id"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.readTaskStep(activeTaskID, x.StepID)
	case state.ToolReadASTNode:
		var x struct {
			Path string `json:"path"`
			Rule string `json:"rule"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.readASTNode(ctx, x.Path, x.Rule)
	case state.ToolPatchASTNode:
		var x struct {
			Path    string `json:"path"`
			Rule    string `json:"rule"`
			Rewrite string `json:"rewrite"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.patchASTNode(ctx, x.Path, x.Rule, x.Rewrite)
	case state.ToolCreateDir:
		var x struct{ Path string }
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.createDir(x.Path)
	case state.ToolDeleteFile:
		var x struct{ Path string }
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.deleteFile(x.Path)
	case state.ToolDeleteDir:
		var x struct{ Path string }
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.deleteDir(x.Path)
	case state.ToolExecuteProcess:
		var x struct {
			Binary         string   `json:"binary"`
			Args           []string `json:"args"`
			TimeoutSeconds int      `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.runProcess(ctx, x.Binary, x.Args, x.TimeoutSeconds)
	case state.ToolExecuteShell:
		var x struct {
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.runShell(ctx, x.Command, x.TimeoutSeconds)
	case state.ToolGitStatus:
		return m.runProcess(ctx, "git", []string{"status", "--short", "--branch"}, 30)
	case state.ToolGitDiff:
		var x struct {
			Staged bool `json:"staged"`
		}
		_ = json.Unmarshal(req.Input, &x)
		args := []string{"diff"}
		if x.Staged {
			args = append(args, "--cached")
		}
		return m.runProcess(ctx, "git", args, 30)
	case state.ToolReadToolOutput:
		var x struct {
			ToolCallID    uint64 `json:"tool_call_id"`
			Offset, Limit int
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.readOutput(x.ToolCallID, x.Offset, x.Limit)
	case state.ToolAnalyzeLargeFile:
		var x struct {
			Path         string `json:"path"`
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.analyzeLargeFile(ctx, x.Path, x.Instructions)
	case state.ToolArtifactList:
		return m.artifactList(activeTaskID)
	case state.ToolArtifactRead:
		var x struct {
			Name                string `json:"name"`
			StartLine, EndLine  int
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.artifactRead(activeTaskID, x.Name, x.StartLine, x.EndLine)
	case state.ToolArtifactWrite:
		var x struct{ Name, Content string }
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.artifactWrite(activeTaskID, x.Name, x.Content)
	case state.ToolArtifactAppend:
		var x struct{ Name, Content string }
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.artifactAppend(activeTaskID, x.Name, x.Content)
	case state.ToolArtifactPatch:
		var x struct {
			Name, Old, New string
			ReplaceAll     bool `json:"replace_all"`
		}
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.artifactPatch(ctx, activeTaskID, x.Name, x.Old, x.New, x.ReplaceAll)
	case state.ToolArtifactSearch:
		var x struct{ Query string }
		if err := json.Unmarshal(req.Input, &x); err != nil {
			return "", nil, err
		}
		return m.artifactSearch(activeTaskID, x.Query)
	default:
		return "", nil, fmt.Errorf("unknown tool kind %q", req.Kind)
	}
}

func (m *Manager) resolveExisting(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", errors.New("path must be project-relative")
	}
	root, err := filepath.EvalSymlinks(m.Root)
	if err != nil {
		return "", err
	}
	root, _ = filepath.Abs(root)
	joined := filepath.Join(root, filepath.Clean(rel))
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", err
	}
	real, _ = filepath.Abs(real)
	if !within(root, real) {
		return "", errors.New("path escapes project root")
	}
	return real, nil
}
func (m *Manager) resolveCreate(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", errors.New("path must be project-relative")
	}
	root, err := filepath.EvalSymlinks(m.Root)
	if err != nil {
		return "", err
	}
	root, _ = filepath.Abs(root)
	joined := filepath.Join(root, filepath.Clean(rel))
	if _, statErr := os.Lstat(joined); statErr == nil {
		real, evalErr := filepath.EvalSymlinks(joined)
		if evalErr != nil {
			return "", evalErr
		}
		real, _ = filepath.Abs(real)
		if !within(root, real) {
			return "", errors.New("path escapes project root")
		}
		return real, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	parent := filepath.Dir(joined)
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	realParent, _ = filepath.Abs(realParent)
	if !within(root, realParent) {
		return "", errors.New("path escapes project root")
	}
	return filepath.Join(realParent, filepath.Base(joined)), nil
}
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (m *Manager) readFile(rel string) (string, *int, error) {
	p, e := m.resolveExisting(rel)
	if e != nil {
		return "", nil, e
	}
	if isBinaryFile(p) {
		return "", nil, fmt.Errorf("cannot read file: %s is a binary file", rel)
	}
	b, e := os.ReadFile(p)
	return string(b), nil, e
}
func (m *Manager) readRange(rel string, start, end int) (string, *int, error) {
	p, e := m.resolveExisting(rel)
	if e != nil {
		return "", nil, e
	}
	if isBinaryFile(p) {
		return "", nil, fmt.Errorf("cannot read file: %s is a binary file", rel)
	}
	if start < 1 {
		start = 1
	}
	if end < start {
		return "", nil, errors.New("end must be >= start")
	}
	f, e := os.Open(p)
	if e != nil {
		return "", nil, e
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var b strings.Builder
	line := 0
	for sc.Scan() {
		line++
		if line < start {
			continue
		}
		if line > end {
			break
		}
		fmt.Fprintf(&b, "%d: %s\n", line, sc.Text())
	}
	return b.String(), nil, sc.Err()
}
func ignored(path string, d fs.DirEntry) bool {
	name := d.Name()
	if d.IsDir() {
		switch name {
		case ".git", ".loa", "node_modules", "vendor", "dist", "build":
			return true
		}
	}
	return false
}
func (m *Manager) searchPath(q string, limit int) (string, *int, error) {
	if limit <= 0 {
		limit = 100
	}
	q = strings.ToLower(q)
	var out []string
	e := filepath.WalkDir(m.Root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if p != m.Root && ignored(p, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(m.Root, p)
		if rel != "." && strings.Contains(strings.ToLower(filepath.ToSlash(rel)), q) {
			out = append(out, filepath.ToSlash(rel))
			if len(out) >= limit {
				return io.EOF
			}
		}
		return nil
	})
	if errors.Is(e, io.EOF) {
		e = nil
	}
	return strings.Join(out, "\n"), nil, e
}
func (m *Manager) searchText(pattern string, isRegex bool, limit int) (string, *int, error) {
	if limit <= 0 {
		limit = 100
	}
	var re *regexp.Regexp
	var err error
	if isRegex {
		re, err = regexp.Compile(pattern)
		if err != nil {
			return "", nil, err
		}
	}
	var lines []string
	stop := errors.New("stop")
	err = filepath.WalkDir(m.Root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if p != m.Root && ignored(p, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if isBinaryFile(p) {
			return nil
		}
		f, e := os.Open(p)
		if e != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		n := 0
		for sc.Scan() {
			n++
			txt := sc.Text()
			match := false
			if re != nil {
				match = re.MatchString(txt)
			} else {
				match = strings.Contains(txt, pattern)
			}
			if match {
				rel, _ := filepath.Rel(m.Root, p)
				lines = append(lines, fmt.Sprintf("%s:%d:%s", filepath.ToSlash(rel), n, txt))
				if len(lines) >= limit {
					return stop
				}
			}
		}
		return nil
	})
	if errors.Is(err, stop) {
		err = nil
	}
	return strings.Join(lines, "\n"), nil, err
}
func (m *Manager) findSymbol(q string, limit int) (string, *int, error) {
	if m.Index == nil || !m.Index.Available() {
		return "", nil, errors.New("symbol index unavailable")
	}
	if limit <= 0 {
		limit = 30
	}
	b, e := json.MarshalIndent(m.Index.Find(q, limit), "", "  ")
	return string(b), nil, e
}
func (m *Manager) listSymbols(path string) (string, *int, error) {
	if m.Index == nil || !m.Index.Available() {
		return "", nil, errors.New("symbol index unavailable")
	}
	b, e := json.MarshalIndent(m.Index.SymbolsForFile(path), "", "  ")
	return string(b), nil, e
}

func (m *Manager) astGrepPath() string {
	if p, err := exec.LookPath("ast-grep"); err == nil {
		return p
	}
	if p, err := exec.LookPath("sg"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p, err := exec.LookPath(filepath.Join(home, ".loa", "bin", "ast-grep")); err == nil {
			return p
		}
	}
	return "ast-grep"
}

func (m *Manager) readASTNode(ctx context.Context, path, rule string) (string, *int, error) {
	rel, err := m.resolveExisting(path)
	if err != nil {
		return "", nil, err
	}
	if isBinaryFile(rel) {
		return "", nil, errors.New("cannot run AST tools on binary files")
	}
	tmp, err := os.CreateTemp("", "sg-rule-*.yml")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(tmp.Name())
	
	ruleYAML := fmt.Sprintf("id: dynamic-rule\nlanguage: go\nrule:\n  %s\n", strings.ReplaceAll(rule, "\n", "\n  "))
	if _, err := tmp.WriteString(ruleYAML); err != nil {
		return "", nil, err
	}
	tmp.Close()
	return m.runProcess(ctx, m.astGrepPath(), []string{"run", "-r", tmp.Name(), rel}, 30)
}

func (m *Manager) patchASTNode(ctx context.Context, path, rule, rewrite string) (string, *int, error) {
	rel, err := m.resolveExisting(path)
	if err != nil {
		return "", nil, err
	}
	if isBinaryFile(rel) {
		return "", nil, errors.New("cannot run AST tools on binary files")
	}
	tmp, err := os.CreateTemp("", "sg-rule-*.yml")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(tmp.Name())
	
	ruleYAML := fmt.Sprintf("id: dynamic-patch\nlanguage: go\nrule:\n  %s\nfix: |\n  %s\n", strings.ReplaceAll(rule, "\n", "\n  "), strings.ReplaceAll(rewrite, "\n", "\n  "))
	if _, err := tmp.WriteString(ruleYAML); err != nil {
		return "", nil, err
	}
	tmp.Close()
	
	out, exit, err := m.runProcess(ctx, m.astGrepPath(), []string{"run", "-r", tmp.Name(), "-U", rel}, 30)
	if err != nil {
		return out, exit, err
	}
	
	msg := fmt.Sprintf("patched AST node in %s", path)
	if m.Index != nil && m.Index.Available() {
		if err := m.Index.RefreshFile(ctx, path); err != nil {
			msg += "; warning: symbol index refresh failed: " + err.Error()
		}
	}
	return msg + "\nOutput:\n" + out, exit, nil
}
func (m *Manager) writeFile(ctx context.Context, rel, content string) (string, *int, error) {
	p, e := m.resolveCreate(rel)
	if e != nil {
		return "", nil, e
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(p); err == nil {
		mode = info.Mode().Perm()
	}
	if e = atomicWrite(p, []byte(content), mode); e != nil {
		return "", nil, e
	}
	msg := fmt.Sprintf("wrote %d bytes to %s", len(content), rel)
	if m.Index != nil && m.Index.Available() {
		if err := m.Index.RefreshFile(ctx, rel); err != nil {
			msg += "; warning: symbol index refresh failed and stale entry was invalidated: " + err.Error()
		}
	}
	return msg, nil, nil
}

func (m *Manager) patchFile(ctx context.Context, rel, old, newStr string, all bool) (string, *int, error) {
	p, e := m.resolveExisting(rel)
	if e != nil {
		return "", nil, e
	}
	if isBinaryFile(p) {
		return "", nil, fmt.Errorf("cannot patch file: %s is a binary file", rel)
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return "", nil, e
	}
	original := string(b)
	count := strings.Count(original, old)
	
	updated := original
	replacements := 1

	if count == 0 {
		if all {
			return "", nil, errors.New("patch target not found (replace_all is not supported for fuzzy patching)")
		}
		var fuzzyErr error
		updated, fuzzyErr = fuzzyPatchLineWindow(original, old, newStr)
		if fuzzyErr != nil {
			return "", nil, fmt.Errorf("patch target not found (exact match failed, fuzzy match failed: %w)", fuzzyErr)
		}
	} else {
		if !all && count != 1 {
			return "", nil, fmt.Errorf("patch target occurs %d times; make patch unique or set replace_all", count)
		}
		if all {
			updated = strings.ReplaceAll(original, old, newStr)
			replacements = count
		} else {
			updated = strings.Replace(original, old, newStr, 1)
		}
	}

	info, e := os.Stat(p)
	if e != nil {
		return "", nil, e
	}
	if e = atomicWrite(p, []byte(updated), info.Mode().Perm()); e != nil {
		return "", nil, e
	}

	msg := ""
	if count == 0 {
		msg = fmt.Sprintf("patched %s (fuzzy line match)", rel)
	} else {
		msg = fmt.Sprintf("patched %s (%d replacement(s))", rel, replacements)
	}

	if m.Index != nil && m.Index.Available() {
		if err := m.Index.RefreshFile(ctx, rel); err != nil {
			msg += "; warning: symbol index refresh failed and stale entry was invalidated: " + err.Error()
		}
	}
	return msg, nil, nil
}

func fuzzyPatchLineWindow(original, old, newStr string) (string, error) {
	origLines := strings.Split(original, "\n")
	oldLinesRaw := strings.Split(old, "\n")
	newLines := strings.Split(newStr, "\n")

	startIdx := 0
	for startIdx < len(oldLinesRaw) && strings.TrimSpace(oldLinesRaw[startIdx]) == "" {
		startIdx++
	}
	endIdx := len(oldLinesRaw) - 1
	for endIdx >= startIdx && strings.TrimSpace(oldLinesRaw[endIdx]) == "" {
		endIdx--
	}
	if startIdx > endIdx {
		return "", errors.New("old block is empty or only whitespace")
	}
	oldLines := oldLinesRaw[startIdx : endIdx+1]
	oldBlockStr := strings.Join(oldLines, "\n")

	targetLen := len(oldLines)
	if targetLen < 2 {
		return "", errors.New("old block must be at least 2 lines for fuzzy matching")
	}

	bestScore := -1.0
	var bestWindows []int

	for i := 0; i <= len(origLines)-targetLen; i++ {
		j := i + targetLen - 1
		matches := 0
		origIdx := i
		for _, oldLine := range oldLines {
			oldT := strings.TrimSpace(oldLine)
			if oldT == "" {
				continue
			}
			for k := origIdx; k <= j; k++ {
				if strings.TrimSpace(origLines[k]) == oldT {
					matches++
					origIdx = k + 1
					break
				}
			}
		}

		nonEmptyOld := 0
		for _, l := range oldLines {
			if strings.TrimSpace(l) != "" {
				nonEmptyOld++
			}
		}
		if nonEmptyOld == 0 {
			continue
		}
		score := float64(matches) / float64(nonEmptyOld)

		if score > bestScore {
			bestScore = score
			bestWindows = []int{i}
		} else if score == bestScore && score > 0 {
			bestWindows = append(bestWindows, i)
		}
	}

	if bestScore < 0.8 {
		return "", fmt.Errorf("best score: %.2f", bestScore)
	}

	bestStart := bestWindows[0]
	if len(bestWindows) > 1 {
		bestDist := -1
		for _, i := range bestWindows {
			j := i + targetLen - 1
			candidateStr := strings.Join(origLines[i:j+1], "\n")
			dist := levenshteinDistance(candidateStr, oldBlockStr)
			if bestDist == -1 || dist < bestDist {
				bestDist = dist
				bestStart = i
			}
		}
	}

	var result []string
	result = append(result, origLines[:bestStart]...)
	result = append(result, newLines...)
	result = append(result, origLines[bestStart+targetLen:]...)

	return strings.Join(result, "\n"), nil
}

func levenshteinDistance(s1, s2 string) int {
	r1, r2 := []rune(s1), []rune(s2)
	n, m := len(r1), len(r2)
	if n == 0 {
		return m
	}
	if m == 0 {
		return n
	}
	d := make([][]int, n+1)
	for i := range d {
		d[i] = make([]int, m+1)
		d[i][0] = i
	}
	for j := 0; j <= m; j++ {
		d[0][j] = j
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			cost := 1
			if r1[i-1] == r2[j-1] {
				cost = 0
			}
			min1 := d[i-1][j] + 1
			min2 := d[i][j-1] + 1
			min3 := d[i-1][j-1] + cost
			
			mVal := min1
			if min2 < mVal { mVal = min2 }
			if min3 < mVal { mVal = min3 }
			d[i][j] = mVal
		}
	}
	return d[n][m]
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".loa-write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func (m *Manager) createDir(rel string) (string, *int, error) {
	p, e := m.resolveCreate(rel)
	if e != nil {
		return "", nil, e
	}
	e = os.MkdirAll(p, 0o755)
	return "created directory " + rel, nil, e
}
func (m *Manager) deleteFile(rel string) (string, *int, error) {
	p, e := m.resolveExisting(rel)
	if e != nil {
		return "", nil, e
	}
	e = os.Remove(p)
	if e == nil && m.Index != nil {
		m.Index.RemoveFile(rel)
	}
	return "deleted file " + rel, nil, e
}
func (m *Manager) deleteDir(rel string) (string, *int, error) {
	p, e := m.resolveExisting(rel)
	if e != nil {
		return "", nil, e
	}
	if filepath.Clean(p) == filepath.Clean(m.Root) {
		return "", nil, errors.New("refusing to delete project root")
	}
	e = os.RemoveAll(p)
	return "deleted directory " + rel, nil, e
}
func (m *Manager) timeout(sec int) time.Duration {
	if sec <= 0 {
		sec = m.Config().CommandTimeoutSeconds
	}
	return time.Duration(sec) * time.Second
}
func (m *Manager) runProcess(ctx context.Context, binary string, args []string, sec int) (string, *int, error) {
	if binary == "" {
		return "", nil, errors.New("binary is required")
	}
	cctx, cancel := context.WithTimeout(ctx, m.timeout(sec))
	defer cancel()
	cmd := exec.CommandContext(cctx, binary, args...)
	cmd.Dir = m.Root
	// Make process and shell execution use the same environment explicitly.
	// In particular, execute_shell must not rely on login-shell startup files
	// that may replace PATH and make binaries visible to execute_process disappear.
	cmd.Env = os.Environ()
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	code := 0
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return b.String(), nil, fmt.Errorf("command timed out after %s", m.timeout(sec))
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			return b.String(), &code, fmt.Errorf("process exited with code %d", code)
		}
		return b.String(), nil, err
	}
	return b.String(), &code, nil
}
func (m *Manager) runShell(ctx context.Context, command string, sec int) (string, *int, error) {
	// Use a non-login shell so it inherits Loa's environment unchanged. A login
	// shell may source /etc/profile or user startup files and replace PATH, which
	// makes execute_shell behave differently from execute_process.
	return m.runProcess(ctx, "/bin/sh", []string{"-c", command}, sec)
}
func (m *Manager) readOutput(id uint64, offset, limit int) (string, *int, error) {
	m.mu.RLock()
	s, ok := m.output[id]
	m.mu.RUnlock()
	if !ok {
		return "", nil, errors.New("tool output not found")
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 16 * 1024
	}
	if offset >= len(s) {
		return "", nil, nil
	}
	end := offset + limit
	if end > len(s) {
		end = len(s)
	}
	return s[offset:end], nil, nil
}
func (m *Manager) analyzeLargeFile(ctx context.Context, rel, instructions string) (string, *int, error) {
	if m.LLM == nil {
		return "", nil, errors.New("LLM client not available")
	}
	p, e := m.resolveExisting(rel)
	if e != nil {
		return "", nil, e
	}
	if isBinaryFile(p) {
		return "", nil, fmt.Errorf("cannot analyze file: %s is a binary file", rel)
	}
	chunks, e := ChunkFileByLines(p, 500)
	if e != nil {
		return "", nil, e
	}
	summary := ""
	for i, c := range chunks {
		sys := "You are an expert code analyst summarizing a large file in chunks. Follow the user instructions carefully."
		var previous string
		if summary == "" {
			previous = "None (this is the first chunk)."
		} else {
			previous = summary
		}
		user := fmt.Sprintf("INSTRUCTIONS:\n%s\n\nPREVIOUS SUMMARY:\n%s\n\nCURRENT CHUNK (lines %d-%d):\n```\n%s\n```\n\nOutput ONLY the updated summary covering both previous content and the new chunk. If no new relevant information is in the chunk, keep the previous summary.", instructions, previous, c.StartLine, c.EndLine, c.Content)
		
		updated, err := m.LLM.ChatText(ctx, m.Config().ModelExecuting, sys, user)
		if err != nil {
			return "", nil, fmt.Errorf("chunk %d analysis failed: %w", i+1, err)
		}
		summary = strings.TrimSpace(updated)
	}
	return summary, nil, nil
}

func ToolSchema(readOnly bool) string {
	schema := `read_file {"path":"relative/path"}
read_range {"path":"relative/path","start":1,"end":120}
search_path {"query":"name fragment","limit":100}
search_text {"pattern":"text or regex","regex":false,"limit":100}
find_symbol {"query":"SymbolName","limit":30}
list_symbols {"path":"relative/source.go"}
read_ast_node {"path":"relative/source.go","rule":"{pattern: 'func $NAME() { $$$BODY }'}"}`

	if !readOnly {
		schema += `
write_file {"path":"relative/path","content":"complete file content"}
patch_file {"path":"relative/path","old":"exact old text","new":"replacement","replace_all":false}
patch_ast_node {"path":"relative/source.go","rule":"{pattern: 'func $NAME() { $$$BODY }'}","rewrite":"func $NAME() error { return nil }"}
create_directory {"path":"relative/path"}
delete_file {"path":"relative/path"}
delete_directory {"path":"relative/path"}`
	}

	schema += `
execute_process {"binary":"go","args":["test","./..."],"timeout_seconds":0}
execute_shell {"command":"go test ./... | tee test.log","timeout_seconds":0}
git_status {}
git_diff {"staged":false}
read_tool_output {"tool_call_id":123,"offset":0,"limit":16384}
analyze_large_file {"path":"relative/path","instructions":"flat string without nested JSON objects"}
artifact_list {}
artifact_read {"name":"filename.md","start_line":0,"end_line":0}`

	if !readOnly {
		schema += `
artifact_write {"name":"filename.md","content":"complete content"}
artifact_append {"name":"filename.md","content":"content to append"}
artifact_patch {"name":"filename.md","old":"exact old text","new":"replacement","replace_all":false}`
	}

	schema += `
artifact_search {"query":"search text"}
search_task_steps {}
read_task_step {"step_id":123}`

	return schema
}

func ParseInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func ValidKind(kind state.ToolKind) bool {
	switch kind {
	case state.ToolReadFile, state.ToolReadRange, state.ToolSearchPath, state.ToolSearchText,
		state.ToolFindSymbol, state.ToolListSymbols, state.ToolReadASTNode, state.ToolWriteFile, state.ToolPatchFile,
		state.ToolPatchASTNode, state.ToolCreateDir, state.ToolDeleteFile, state.ToolDeleteDir, state.ToolExecuteProcess,
		state.ToolExecuteShell, state.ToolGitStatus, state.ToolGitDiff, state.ToolReadToolOutput, state.ToolAnalyzeLargeFile,
		state.ToolSearchTaskSteps, state.ToolReadTaskStep, state.ToolArtifactList, state.ToolArtifactRead,
		state.ToolArtifactWrite, state.ToolArtifactAppend, state.ToolArtifactPatch, state.ToolArtifactSearch:
		return true
	default:
		return false
	}

}

func (m *Manager) searchTaskSteps(taskID uint64) (string, *int, error) {
	if m.GetTask == nil {
		return "", nil, errors.New("task access not configured")
	}
	task, err := m.GetTask(taskID)
	if err != nil {
		return "", nil, err
	}
	if len(task.StepResults) == 0 {
		return "No completed steps found.", nil, nil
	}
	var out string
	for _, res := range task.StepResults {
		out += fmt.Sprintf("Step ID: %d | Summary: %s\n", res.StepID, res.Summary)
	}
	return out, nil, nil
}

func (m *Manager) readTaskStep(taskID uint64, stepID uint64) (string, *int, error) {
	if m.GetTask == nil {
		return "", nil, errors.New("task access not configured")
	}
	task, err := m.GetTask(taskID)
	if err != nil {
		return "", nil, err
	}
	for _, res := range task.StepResults {
		if res.StepID == stepID {
			b, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				return "", nil, err
			}
			return string(b), nil, nil
		}
	}
	return "", nil, fmt.Errorf("step id %d not found in task results", stepID)
}
