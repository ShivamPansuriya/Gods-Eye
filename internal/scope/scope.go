// Package scope decides, with plain code and no model, what a review covers:
// for every changed file, whether it is hunted for bugs as code, hunted by the
// tests lens, skipped (generated, lockfile, vendored, not code), or a secret
// that no model may ever see; and which review checklist applies to it.
//
// The idea and the built-in lists come from alibaba/open-code-review: hard
// constraints for the steps that must not go wrong, the model for the rest.
package scope

import (
	"encoding/json"
	"path"
	"sort"
	"strings"
)

// Kind is what the review does with a changed file.
type Kind string

const (
	Code   Kind = "code"   // hunted for bugs
	Test   Kind = "test"   // hunted by the tests lens
	Skip   Kind = "skip"   // not hunted; still visible to the mapper and readable by tools
	Secret Kind = "secret" // removed from the diff; no tool may read it
)

// File is the decision for one changed file.
type File struct {
	Path      string `json:"path"`
	Kind      Kind   `json:"kind"`
	Reason    string `json:"reason,omitempty"`    // why it is not plain code: "lockfile", "repo rule: exclude legacy/**"
	Checklist string `json:"checklist,omitempty"` // name of the review checklist that applies ("java", "repo: src/**")
}

// Config is a repository's own scope settings, read from the base branch so a
// PR cannot change how it is reviewed. It accepts both God's Eye's
// .godseye.json keys and open-code-review's .opencodereview/rule.json.
type Config struct {
	Include    []string   `json:"include"`    // review these even if a built-in list would skip them
	Exclude    []string   `json:"exclude"`    // never hunt these
	Checklists []RuleSpec `json:"checklists"` // .godseye.json
	Rules      []RuleSpec `json:"-"`          // .opencodereview/rule.json "rules"
}

// RuleSpec gives files matching Path a checklist. Rule is the text, or a
// repository path to a .md/.txt file holding it.
type RuleSpec struct {
	Path        string `json:"path"`
	Rule        string `json:"rule"`
	MergeSystem bool   `json:"merge_system_rule"`
}

// ParseGodsEye reads the scope keys of .godseye.json.
func ParseGodsEye(b []byte) Config {
	var c Config
	_ = json.Unmarshal(b, &c)
	return c
}

// ParseOCR reads an open-code-review rule.json.
func ParseOCR(b []byte) Config {
	var raw struct {
		Rules   []RuleSpec `json:"rules"`
		Include []string   `json:"include"`
		Exclude []string   `json:"exclude"`
	}
	_ = json.Unmarshal(b, &raw)
	return Config{Include: raw.Include, Exclude: raw.Exclude, Rules: raw.Rules}
}

// Load reads a repository's scope settings from .godseye.json and
// .opencodereview/rule.json; read returns a file at the commit the settings
// should come from (the base branch for a PR).
func Load(read func(string) (string, bool)) Config {
	var c Config
	if s, ok := read(".godseye.json"); ok {
		c = ParseGodsEye([]byte(s))
	}
	if s, ok := read(".opencodereview/rule.json"); ok {
		c = c.Merge(ParseOCR([]byte(s)))
	}
	return c
}

// Merge returns c with o's settings added after c's (c's checklists win).
func (c Config) Merge(o Config) Config {
	var cl []RuleSpec
	for _, s := range [][]RuleSpec{c.Checklists, c.Rules, o.Checklists, o.Rules} {
		cl = append(cl, s...)
	}
	return Config{
		Include:    append(append([]string{}, c.Include...), o.Include...),
		Exclude:    append(append([]string{}, c.Exclude...), o.Exclude...),
		Checklists: cl,
	}
}

// Plan holds every file's decision and the checklist texts they refer to.
type Plan struct {
	Files      []File            `json:"files"`
	Checklists map[string]string `json:"-"` // name → text
	byPath     map[string]int
}

// Decide classifies changed files. binary reports files git marked binary;
// read returns a repository file's text (for checklists kept in .md files) and
// may be nil.
func Decide(paths []string, binary func(string) bool, cfg Config, read func(string) (string, bool)) *Plan {
	include, exclude := compileAll(cfg.Include), compileAll(cfg.Exclude)
	type repoRule struct {
		g     Glob
		name  string
		text  string
		merge bool
	}
	var repo []repoRule
	for _, r := range cfg.Checklists {
		text := strings.TrimSpace(r.Rule)
		if looksLikeFile(text) && read != nil {
			if s, ok := read(text); ok {
				text = s
			}
		}
		if text == "" && !r.MergeSystem {
			continue
		}
		repo = append(repo, repoRule{Compile(r.Path), "repo: " + r.Path, text, r.MergeSystem})
	}

	p := &Plan{Checklists: map[string]string{}, byPath: map[string]int{}}
	for _, fp := range paths {
		f := File{Path: fp, Kind: Code}
		switch {
		case IsSecret(fp):
			f.Kind, f.Reason = Secret, "credential file, never sent to the model"
		case binary != nil && binary(fp):
			f.Kind, f.Reason = Skip, "binary"
		default:
			if g, ok := firstMatch(exclude, fp); ok {
				f.Kind, f.Reason = Skip, "repo rule: exclude "+g.Src
			} else if _, ok := firstMatch(include, fp); !ok {
				f.Kind, f.Reason = builtIn(fp)
			}
		}
		if f.Kind == Code || f.Kind == Test {
			sysName, sysText := systemChecklist(fp)
			for _, r := range repo {
				if !r.g.Match(fp) {
					continue
				}
				name, text := r.name, r.text
				if r.merge && sysText != "" {
					name = r.name + " + " + sysName
					text = sysText + "\n\n### Repository rules\n" + r.text
				}
				sysName, sysText = name, text
				break
			}
			if sysText != "" {
				f.Checklist = sysName
				p.Checklists[sysName] = sysText
			}
		}
		p.byPath[fp] = len(p.Files)
		p.Files = append(p.Files, f)
	}
	return p
}

// Classify is the built-in decision for one path, without repository settings.
func Classify(fp string) (Kind, string) {
	if IsSecret(fp) {
		return Secret, "credential file, never sent to the model"
	}
	return builtIn(fp)
}

func builtIn(fp string) (Kind, string) {
	// Skips first: a test inside vendored or generated code is not ours.
	for _, s := range skips {
		for _, g := range s.globs {
			if g.Match(fp) {
				return Skip, s.reason
			}
		}
	}
	for _, g := range tests {
		if g.Match(fp) {
			return Test, "test"
		}
	}
	if ext := strings.ToLower(path.Ext(fp)); ext != "" && !codeExts[ext] {
		return Skip, "not code (" + ext + ")"
	}
	return Code, ""
}

// Of returns the decision for a path; on a nil plan, or for a path the plan
// does not list, the built-in decision.
func (p *Plan) Of(fp string) File {
	if p != nil {
		if i, ok := p.byPath[fp]; ok {
			return p.Files[i]
		}
	}
	k, why := Classify(fp)
	return File{Path: fp, Kind: k, Reason: why}
}

// Paths returns the paths of the given kinds, in plan order.
func (p *Plan) Paths(kinds ...Kind) map[string]bool {
	out := map[string]bool{}
	for _, f := range p.Files {
		for _, k := range kinds {
			if f.Kind == k {
				out[f.Path] = true
			}
		}
	}
	return out
}

// Summary is what the report shows: how many files of each kind, why files
// were skipped, and which checklists were used.
type Summary struct {
	Code       int            `json:"code"`
	Test       int            `json:"test"`
	Skipped    int            `json:"skipped"`
	Secret     int            `json:"secret"`
	Reasons    map[string]int `json:"reasons,omitempty"`
	Checklists map[string]int `json:"checklists,omitempty"`
	Files      []File         `json:"files"` // every file that is not plain code, plus code files with a checklist
}

// Summary condenses the plan for the report.
func (p *Plan) Summary() *Summary {
	s := &Summary{Reasons: map[string]int{}, Checklists: map[string]int{}}
	for _, f := range p.Files {
		switch f.Kind {
		case Code:
			s.Code++
		case Test:
			s.Test++
		case Skip:
			s.Skipped++
			s.Reasons[reasonKey(f.Reason)]++
		case Secret:
			s.Secret++
		}
		if f.Checklist != "" {
			s.Checklists[f.Checklist]++
		}
		if f.Kind != Code || f.Checklist != "" {
			s.Files = append(s.Files, f)
		}
	}
	sort.SliceStable(s.Files, func(a, b int) bool { return kindRank(s.Files[a].Kind) < kindRank(s.Files[b].Kind) })
	return s
}

func reasonKey(r string) string {
	if strings.HasPrefix(r, "not code") {
		return "not code"
	}
	if strings.HasPrefix(r, "repo rule") {
		return "repo rule"
	}
	return r
}

func kindRank(k Kind) int {
	switch k {
	case Secret:
		return 0
	case Skip:
		return 1
	case Test:
		return 2
	}
	return 3
}

// IsSecret reports credential files: built-in patterns and .env files
// (.env.example, .env.sample and .env.template are not secrets).
func IsSecret(fp string) bool {
	base := strings.ToLower(path.Base(fp))
	switch base {
	case ".env.example", ".env.sample", ".env.template":
		return false
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	_, ok := firstMatch(secrets, fp)
	return ok
}

// looksLikeFile reports a rule that names a .md/.txt file in the repository.
// A path that leaves the repository is rule text, never read.
func looksLikeFile(s string) bool {
	if strings.ContainsAny(s, "\n") || path.IsAbs(s) || strings.Contains(s, "..") {
		return false
	}
	switch strings.ToLower(path.Ext(s)) {
	case ".md", ".txt", ".markdown":
		return true
	}
	return false
}

var (
	secrets = compileAll(secretPatterns)
	tests   = compileAll(testPatterns)
	skips   []struct {
		reason string
		globs  []Glob
	}
)

func init() {
	for _, s := range skipPatterns {
		skips = append(skips, struct {
			reason string
			globs  []Glob
		}{s.Reason, compileAll(s.Patterns)})
	}
}

// Excluded lists the files the review does not hunt (skipped and credential
// files), for an outside engine that must leave them out too.
func (p *Plan) Excluded() []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, f := range p.Files {
		if f.Kind == Skip || f.Kind == Secret {
			out = append(out, f.Path)
		}
	}
	return out
}
