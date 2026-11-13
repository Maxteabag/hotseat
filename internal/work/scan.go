package work

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A quota listing runs every minute on a busy host, over every transcript of the
// last week. Two things keep that cheap: the tail of a transcript is read only as
// far as the answer needs, and an unchanged transcript is not read again.

// Block sizes for reading a transcript backwards. Most transcripts are settled by
// their last record, so the first read is small; long records are fetched in
// growing blocks.
const (
	firstBlock = 8 * 1024
	maxBlock   = 256 * 1024
)

// scanBack hands a transcript's valid records to visit, newest first, until
// visit returns false. It reports false when the file could not be read.
func scanBack(path string, visit func(Entry) bool) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	pos := info.Size()
	size := int64(firstBlock)
	var pending []byte
	for pos > 0 {
		if pos < size {
			size = pos
		}
		pos -= size
		block := make([]byte, int(size), int(size)+len(pending))
		if _, err := file.ReadAt(block, pos); err != nil {
			return false
		}
		block = append(block, pending...)
		end := len(block)
		for {
			i := bytes.LastIndexByte(block[:end], '\n')
			if i < 0 {
				break
			}
			if entry, ok := parseLine(block[i+1 : end]); ok {
				if !visit(entry) {
					return true
				}
			}
			end = i
		}
		pending = block[:end]
		if size < maxBlock {
			size *= 2
		}
	}
	if entry, ok := parseLine(pending); ok {
		visit(entry)
	}
	return true
}

// verdict is what the stopped-work scan concludes about one transcript.
type verdict struct {
	Stopped bool   `json:"stopped,omitempty"`
	CWD     string `json:"cwd,omitempty"`
	Said    string `json:"said,omitempty"`
}

// isTurn says whether a record is an assistant or user turn.
func isTurn(entry Entry) bool {
	kind := entry.str("type")
	return kind == "assistant" || kind == "user"
}

// endsOnLimit says whether a turn is the error Claude Code writes when a usage
// limit stops a session.
func endsOnLimit(last Entry) bool {
	if last == nil || !truthy(last["isApiErrorMessage"]) {
		return false
	}
	message, _ := last["message"].(map[string]any)
	text, ok := message["content"].(string)
	if !ok {
		if encoded, err := json.Marshal(message["content"]); err == nil {
			text = string(encoded)
		}
	}
	return limitText.MatchString(text)
}

// judge decides whether a transcript ends on a usage limit. It reports false
// when the transcript could not be read, so that nothing is remembered about it.
//
// Only the final turn counts: an earlier limit that was worked through is
// history, not something waiting to be continued. So the tail is read just far
// enough to find that turn, and the longer tail holding the last reply is read
// only for the few transcripts that did stop.
func judge(transcript string) (verdict, bool) {
	var last Entry
	seen := 0
	read := scanBack(transcript, func(entry Entry) bool {
		seen++
		if isTurn(entry) {
			last = entry
			return false
		}
		return seen < tailKeep
	})
	if !read {
		return verdict{}, false
	}
	if !endsOnLimit(last) {
		return verdict{}, true
	}
	// The transcript may have grown since the first look, so the answer is taken
	// from this one reading of the tail alone.
	entries := LastEntries(transcript, tailKeep)
	if len(entries) == 0 {
		return verdict{}, false
	}
	last = lastTurn(entries)
	if !endsOnLimit(last) {
		return verdict{}, true
	}
	said := ""
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.str("type") == "assistant" && !truthy(e["isApiErrorMessage"]) && strings.TrimSpace(EntryText(e)) != "" {
			said = EntryText(e)
			break
		}
	}
	cwd := last.str("cwd")
	if cwd == "" {
		cwd = DecodeProjectDir(filepath.Base(filepath.Dir(transcript)))
	}
	return verdict{Stopped: true, CWD: cwd, Said: Trim(said, 150)}, true
}

// scanCacheVersion changes whenever judge would answer differently for the same
// bytes, which makes every remembered verdict stale.
const scanCacheVersion = 1

// ScanCache remembers what the stopped-work scan concluded about each transcript,
// keyed by its size and modification time. Transcripts only grow, so a transcript
// with the same size and time holds the same answer and is not read again.
//
// A nil *ScanCache remembers nothing.
type ScanCache struct {
	// Path is the cache file.
	Path string
	// Warn receives cache-file write failures, which never fail a listing. Nil
	// writes them to standard error.
	Warn func(error)
}

// DefaultScanCachePath is ${XDG_CACHE_HOME:-~/.cache}/hotseat/stopped-scan.json.
func DefaultScanCachePath() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "hotseat", "stopped-scan.json")
}

// DefaultScanCache is the cache a real listing uses.
func DefaultScanCache() *ScanCache {
	return &ScanCache{Path: DefaultScanCachePath()}
}

// scanRecord is one transcript's remembered verdict.
type scanRecord struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mtime"`
	verdict
}

// scanFile is the cache file. Files is keyed by the transcript's path below the
// projects directory.
type scanFile struct {
	Version  int                   `json:"version"`
	Projects string                `json:"projects"`
	Files    map[string]scanRecord `json:"files"`
}

// load returns the remembered verdicts for a projects directory. An absent,
// damaged, outdated or foreign cache reads as empty.
func (c *ScanCache) load(projectsDir string) map[string]scanRecord {
	if c == nil || c.Path == "" {
		return nil
	}
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		return nil
	}
	var stored scanFile
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil
	}
	if stored.Version != scanCacheVersion || stored.Projects != projectsDir {
		return nil
	}
	return stored.Files
}

// store replaces the cache file atomically with mode 0600. A cache needs no
// fsync: a damaged file reads as empty and is rebuilt.
func (c *ScanCache) store(projectsDir string, files map[string]scanRecord) {
	if c == nil || c.Path == "" {
		return
	}
	err := func() error {
		dir := filepath.Dir(c.Path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		data, err := json.Marshal(scanFile{Version: scanCacheVersion, Projects: projectsDir, Files: files})
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(dir, ".stopped-scan-*")
		if err != nil {
			return err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(name, c.Path)
	}()
	if err == nil {
		return
	}
	err = fmt.Errorf("stopped-scan cache: %w", err)
	if c.Warn != nil {
		c.Warn(err)
		return
	}
	fmt.Fprintln(os.Stderr, "hotseat:", err)
}

// ScanStopped lists native sessions whose transcript ends on a usage-limit
// error, newest first, reading only the transcripts that changed since cache
// last saw them. windowDays 0 means no cutoff.
func ScanStopped(cache *ScanCache, projectsDir string, windowDays int, now time.Time) []StoppedItem {
	if info, err := os.Stat(projectsDir); err != nil || !info.IsDir() {
		return nil
	}
	var cutoff float64
	if windowDays != 0 {
		cutoff = epochSeconds(now) - float64(windowDays)*86400
	}
	// Verdicts outside this listing's window are kept while they are still inside
	// the default one, so a narrow listing does not throw away what the regular
	// listing will want a minute later.
	keepFrom := epochSeconds(now) - DefaultWindowDays*86400
	known := cache.load(projectsDir)
	next := make(map[string]scanRecord, len(known))
	changed := false
	var found []StoppedItem
	for _, transcript := range transcripts(projectsDir) {
		info, err := os.Stat(transcript)
		if err != nil {
			continue
		}
		key, err := filepath.Rel(projectsDir, transcript)
		if err != nil {
			key = transcript
		}
		modified := epochSeconds(info.ModTime())
		record, remembered := known[key]
		remembered = remembered && record.Size == info.Size() && record.ModTime == info.ModTime().UnixNano()
		if modified < cutoff {
			if remembered && modified >= keepFrom {
				next[key] = record
			}
			continue
		}
		if !remembered {
			result, ok := judge(transcript)
			if !ok {
				continue
			}
			record = scanRecord{Size: info.Size(), ModTime: info.ModTime().UnixNano(), verdict: result}
			changed = true
		}
		next[key] = record
		if !record.Stopped {
			continue
		}
		id := stem(transcript)
		found = append(found, StoppedItem{
			Kind:        "native",
			Cause:       "usage_limit",
			Pending:     0,
			ID:          id,
			Name:        shortName(id),
			Backend:     "claude",
			CWD:         record.CWD,
			LastMessage: record.Said,
			StoppedAt:   modified,
		})
	}
	if changed || len(next) != len(known) {
		cache.store(projectsDir, next)
	}
	slices.SortStableFunc(found, func(a, b StoppedItem) int {
		return compareFloatDesc(a.StoppedAt, b.StoppedAt)
	})
	return found
}
