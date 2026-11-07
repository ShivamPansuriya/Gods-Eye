package review

import (
	"context"
	"strings"

	"github.com/ShivamPansuriya/gods-eye/internal/ocr"
)

// OCRShare is the part of the hunting budget open-code-review may spend.
// Measured on PR #267 (12 files, Opus 5.5): it found 8 issues for $1.13 in two
// minutes, 2 of them real bugs God's Eye's own hunters missed.
const OCRShare = 0.15

// ocrUSDPerMTokens converts that share into ocr's token budget. Most of its
// tokens are cache reads: 570k tokens cost $1.13 on PR #267.
const ocrUSDPerMTokens = 2.0

// OCRRequest is one open-code-review run.
type OCRRequest struct {
	From      string // first commit ("" = the PR's merge base); the PR head is the last
	Model     string
	MaxTokens int64    // total token budget
	Exclude   []string // files it must not read: skipped and credential files
}

// OCRFunc runs open-code-review.
type OCRFunc func(ctx context.Context, req OCRRequest) (*ocr.Result, error)

// fromOCR turns open-code-review comments into candidate findings. Style and
// documentation comments are dropped: God's Eye reports defects only.
func fromOCR(cs []ocr.Comment, round int) []Finding {
	var out []Finding
	for _, c := range cs {
		switch strings.ToLower(c.Category) {
		case "style", "documentation":
			continue
		}
		text := strings.TrimSpace(c.Content)
		if c.Path == "" || text == "" {
			continue
		}
		quote := firstCodeLine(c.ExistingCode)
		f := Finding{
			Title: clampWords(firstSentence(text), 12), File: c.Path, Line: c.StartLine, EndLine: c.EndLine,
			Quote: quote, Scenario: clampWords(text, 120), Category: ocrCategory(c.Category),
			Severity: "minor", Confidence: "medium", BugClass: strings.ToLower(c.Category),
			Source: "ocr", Round: round,
		}
		switch strings.ToLower(c.Severity) {
		case "critical", "high":
			f.Severity, f.Confidence = "important", "high"
		case "low":
			f.Confidence = "low"
		}
		if strings.TrimSpace(c.SuggestionCode) != "" && strings.TrimSpace(c.ExistingCode) != "" {
			f.Suggestion = c.SuggestionCode
		}
		out = append(out, f)
	}
	return out
}

// OCREffort maps God's Eye's effort to open-code-review's (its review rounds).
func OCREffort(e string) string {
	switch e {
	case "low":
		return "low"
	case "xhigh", "max":
		return "high"
	}
	return "medium"
}

func ocrCategory(c string) string {
	switch strings.ToLower(c) {
	case "security":
		return "security"
	case "performance":
		return "performance"
	}
	return "logic"
}

func firstCodeLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "+")); t != "" {
			return t
		}
	}
	return ""
}

// firstSentence cuts text at its first sentence end, ignoring dots inside code
// spans (`a.b()`).
func firstSentence(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	inCode := false
	for i, r := range s {
		switch {
		case r == '`':
			inCode = !inCode
		case !inCode && (r == '.' || r == '?' || r == '!') && (i+1 == len(s) || s[i+1] == ' '):
			return s[:i]
		}
	}
	return s
}
