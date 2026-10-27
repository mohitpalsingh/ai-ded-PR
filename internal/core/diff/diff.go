package diff

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"

	"github.com/mohitpalsingh/ai-ded-pr/internal/core/models"
)

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

// ParsePatch parses a unified-diff patch (one file) into hunks.
func ParsePatch(patch string) []models.Hunk {
	var hunks []models.Hunk
	sc := bufio.NewScanner(strings.NewReader(patch))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var cur *models.Hunk
	oldNum, newNum := 0, 0
	for sc.Scan() {
		line := sc.Text()
		m := hunkRe.FindStringSubmatch(line)
		if m != nil {
			if cur != nil {
				hunks = append(hunks, *cur)
			}
			oc := 1
			nc := 1
			if m[2] != "" {
				fmt.Sscan(m[2], &oc)
			}
			if m[4] != "" {
				fmt.Sscan(m[4], &nc)
			}
			fmt.Sscan(m[1], &oldNum)
			fmt.Sscan(m[3], &newNum)
			cur = &models.Hunk{
				OldStart: oldNum,
				OldCount: oc,
				NewStart: newNum,
				NewCount: nc,
				Header:   strings.TrimSpace(m[5]),
			}
			continue
		}
		if cur == nil {
			continue
		}
		if line == "" {
			// treat as context? skip trailing empties
			continue
		}
		prefix := line[:1]
		content := ""
		if len(line) > 1 {
			content = line[1:]
		}
		switch prefix {
		case " ":
			cur.Lines = append(cur.Lines, models.DiffLine{Type: models.DiffContext, OldNum: oldNum, NewNum: newNum, Content: content})
			oldNum++
			newNum++
		case "+":
			cur.Lines = append(cur.Lines, models.DiffLine{Type: models.DiffAdd, NewNum: newNum, Content: content})
			newNum++
		case "-":
			cur.Lines = append(cur.Lines, models.DiffLine{Type: models.DiffRemove, OldNum: oldNum, Content: content})
			oldNum++
		case "\\":
			// "\ No newline at end of file"
			continue
		}
	}
	if cur != nil {
		hunks = append(hunks, *cur)
	}
	return hunks
}

// ParseUnifiedDiff parses the output of `git diff` / unified diff into changed files.
func ParseUnifiedDiff(text string) []models.ChangedFile {
	var files []models.ChangedFile
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)

	var cur *models.ChangedFile
	var patchLines []string
	flush := func() {
		if cur == nil {
			return
		}
		cur.Patch = strings.Join(patchLines, "\n")
		cur.Hunks = ParsePatch(cur.Patch)
		recomputeStats(cur)
		files = append(files, *cur)
		cur = nil
		patchLines = nil
	}

	diffGitRe := regexp.MustCompile(`^diff --git a/(.*) b/(.*)$`)
	for sc.Scan() {
		line := sc.Text()
		if m := diffGitRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &models.ChangedFile{Path: m[2], OldPath: m[1], Status: "modified"}
			patchLines = []string{line}
			continue
		}
		if cur == nil {
			continue
		}
		// meta
		if strings.HasPrefix(line, "new file mode") {
			cur.Status = "added"
			patchLines = append(patchLines, line)
			continue
		}
		if strings.HasPrefix(line, "deleted file mode") {
			cur.Status = "deleted"
			patchLines = append(patchLines, line)
			continue
		}
		if strings.HasPrefix(line, "rename from ") {
			cur.OldPath = strings.TrimPrefix(line, "rename from ")
			cur.Status = "renamed"
			patchLines = append(patchLines, line)
			continue
		}
		if strings.HasPrefix(line, "rename to ") {
			cur.Path = strings.TrimPrefix(line, "rename to ")
			patchLines = append(patchLines, line)
			continue
		}
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") ||
			strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "old mode") ||
			strings.HasPrefix(line, "new mode") || strings.HasPrefix(line, "similarity index") ||
			strings.HasPrefix(line, "Binary files") {
			if strings.HasPrefix(line, "Binary files") {
				cur.Binary = true
			}
			patchLines = append(patchLines, line)
			continue
		}
		if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, " ") ||
			strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			patchLines = append(patchLines, line)
			continue
		}
		// other
	}
	flush()
	// Language detection
	for i := range files {
		files[i].Language = LanguageFromPath(files[i].Path)
	}
	return files
}

func recomputeStats(f *models.ChangedFile) {
	add, del := 0, 0
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			switch l.Type {
			case models.DiffAdd:
				add++
			case models.DiffRemove:
				del++
			}
		}
	}
	f.Additions = add
	f.Deletions = del
}

var extLang = map[string]string{
	".go":    "Go",
	".py":    "Python",
	".js":    "JavaScript",
	".ts":    "TypeScript",
	".tsx":   "TypeScript",
	".rs":    "Rust",
	".java":  "Java",
	".kt":    "Kotlin",
	".c":     "C",
	".h":     "C",
	".cpp":   "C++",
	".cc":    "C++",
	".rb":    "Ruby",
	".swift": "Swift",
	".md":    "Markdown",
	".json":  "JSON",
	".yaml":  "YAML",
	".yml":   "YAML",
	".sh":    "Shell",
}

func LanguageFromPath(p string) string {
	i := strings.LastIndex(p, ".")
	if i < 0 {
		return ""
	}
	if v, ok := extLang[strings.ToLower(p[i:])]; ok {
		return v
	}
	return ""
}
