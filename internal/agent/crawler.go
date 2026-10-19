package agent

import (
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/laughingmandev/loa/internal/agent/primitives"
	"github.com/laughingmandev/loa/internal/index"
	"github.com/laughingmandev/loa/internal/state"
)

// RunCrawler synchronously crawls the ctags index, extracts narrative frames
// for unindexed files, and saves them to the LTM database.
// This function locks the engine to prevent concurrent LLM inferences.
func (e *Engine) RunCrawler(ctx context.Context) error {
	if e.index == nil {
		return nil
	}

	e.log(state.LogSystem, "Crawler", "starting synchronous LTM crawler", "", "")

	// Get all indexed files from ctags
	idxFiles := e.index.Snapshot()

	// Find already indexed files in memStore
	indexed := make(map[string]bool)
	for _, item := range e.memStore.Items() {
		if item.Kind == state.MemoryStructural {
			for _, anchor := range item.Anchors {
				indexed[anchor] = true
			}
		}
	}

	for file, syms := range idxFiles {
		if indexed[file] {
			continue
		}

		absPath := filepath.Join(e.root, file)
		b, fileErr := os.ReadFile(absPath)
		if fileErr != nil {
			e.log(state.LogSystem, "Crawler", "failed to read file for hash", file, fileErr.Error())
			continue
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		fileHash := fmt.Sprintf("%08x", crc32.ChecksumIEEE(b))

		chunks, err := e.chunkFile(file, syms)
		if err != nil {
			e.log(state.LogSystem, "Crawler", "failed to chunk file", file, err.Error())
			continue
		}

		for i, chunk := range chunks {
			purpose := fmt.Sprintf("CHUNK %d of %s:\n%s", i+1, file, chunk)
			nfPrim := primitives.NewNarrativeFramePrimitive(e.cfg.Get(), e.llm, baseSystem)
			frame, err := nfPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.NarrativeFrameInput{Purpose: purpose})
			if err != nil {
				e.log(state.LogSystem, "Crawler", "failed to generate narrative frame", file, err.Error())
				continue
			}
			e.log(state.LogSystem, "Crawler", fmt.Sprintf("extracted AST narrative frame for chunk %d of %s", i+1, file), chunk, frame.Frame)

			// Extract Keywords
			extPrim := primitives.NewExtractKeywordsPrimitive(e.cfg.Get(), e.llm, baseSystem)
			ext, err := extPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ExtractKeywordsInput{Text: frame.Frame})
			if err != nil {
				e.log(state.LogSystem, "ExtractKeywords", "keyword extraction failed", file, err.Error())
				continue
			}

			// Create Embedding
			var emb []float32
			if len(ext.IndexKeys) > 0 {
				emb, _ = e.tryEmbed(ctx, strings.Join(ext.IndexKeys, " "))
			}

			// Save to LTM
			e.memStore.AddMemory(state.MemoryItem{
				Kind:      state.MemoryStructural,
				Text:      frame.Frame,
				Anchors:   []string{file},
				IndexKeys: ext.IndexKeys,
				FileHash:  fileHash,
				Embedding: emb,
				CreatedAt: time.Now(),
			})
		}
	}

	e.log(state.LogSystem, "Crawler", "completed synchronous LTM crawler", "", "")
	return nil
}

func (e *Engine) chunkFile(relPath string, syms []index.Symbol) ([]string, error) {
	absPath := filepath.Join(e.root, relPath)
	b, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	content := string(b)
	lines := strings.Split(content, "\n")

	if len(lines) == 0 {
		return nil, nil
	}

	// If the file is relatively small, just return the whole file as a single chunk.
	if len(lines) < e.cfg.Get().CrawlerSmallFileThreshold || len(syms) == 0 {
		return []string{content}, nil
	}

	// We use ctags symbols to define logical boundaries.
	// Sort symbols by starting line.
	sort.Slice(syms, func(i, j int) bool {
		return syms[i].Line < syms[j].Line
	})

	type bound struct {
		start int
		end   int
	}
	var bounds []bound

	// Group symbols into non-overlapping bounds
	for _, s := range syms {
		start := s.Line
		end := s.EndLine
		if end == 0 {
			// If no end line, approximate it to a small block.
			end = start + 10
		}
		if end > len(lines) {
			end = len(lines)
		}

		if len(bounds) == 0 {
			bounds = append(bounds, bound{start, end})
			continue
		}

		last := &bounds[len(bounds)-1]
		if start <= last.end {
			// Overlapping, extend the last bound
			if end > last.end {
				last.end = end
			}
		} else {
			// If gap is small (e.g. < 5 lines), merge them to avoid too many tiny chunks
			if start-last.end < 5 {
				last.end = end
			} else {
				bounds = append(bounds, bound{start, end})
			}
		}
	}

	// Merge bounds into larger chunks (e.g., at least 50 lines per chunk) to avoid tiny frames
	var chunks []string
	var currentLines []string
	currentLineCount := 0

	for _, b := range bounds {
		// Lines are 1-indexed for ctags
		startIdx := b.start - 1
		endIdx := b.end
		if startIdx < 0 {
			startIdx = 0
		}
		if endIdx > len(lines) {
			endIdx = len(lines)
		}
		if startIdx >= endIdx {
			continue
		}

		chunkContent := strings.Join(lines[startIdx:endIdx], "\n")
		currentLines = append(currentLines, chunkContent)
		currentLineCount += (endIdx - startIdx)

		if currentLineCount > e.cfg.Get().CrawlerMaxChunkLines {
			chunks = append(chunks, strings.Join(currentLines, "\n\n...\n\n"))
			currentLines = nil
			currentLineCount = 0
		}
	}

	if len(currentLines) > 0 {
		chunks = append(chunks, strings.Join(currentLines, "\n\n...\n\n"))
	}

	// If something failed and we have no chunks, fallback to the whole file
	if len(chunks) == 0 {
		return []string{content}, nil
	}

	return chunks, nil
}
