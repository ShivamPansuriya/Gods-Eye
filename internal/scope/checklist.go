package scope

import (
	"bytes"
	"embed"
	"encoding/json"
	"strings"
)

// The checklists are open-code-review's per-language review rules
// (internal/config/rules/rule_docs, Apache-2.0; see NOTICE), matched to files
// by the same path map. Their generic default is left out: God's Eye's hunter
// lenses already cover it.
//
//go:embed checklists/*.md checklists/system_rules.json
var checklistFS embed.FS

type pathRule struct {
	g    Glob
	name string
	file string
}

var systemRules = loadSystemRules()

// loadSystemRules reads path_rule_map in file order: the first match wins, so
// the order of the JSON object is part of its meaning.
func loadSystemRules() []pathRule {
	b, err := checklistFS.ReadFile("checklists/system_rules.json")
	if err != nil {
		panic(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var out []pathRule
	inMap := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := tok.(string)
		if !ok {
			continue
		}
		if key == "path_rule_map" {
			inMap = true
			continue
		}
		if !inMap {
			continue
		}
		var file string
		if err := dec.Decode(&file); err != nil {
			continue
		}
		out = append(out, pathRule{Compile(key), strings.TrimSuffix(file, ".md"), file})
	}
	return out
}

// systemChecklist returns the built-in checklist for a path, if any.
func systemChecklist(fp string) (string, string) {
	for _, r := range systemRules {
		if !r.g.Match(fp) {
			continue
		}
		b, err := checklistFS.ReadFile("checklists/" + r.file)
		if err != nil {
			return "", ""
		}
		return r.name, strings.TrimSpace(string(b))
	}
	return "", ""
}
