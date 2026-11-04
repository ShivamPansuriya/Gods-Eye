// Package risk applies deterministic path rules on top of the model's judgement.
// The article's rule: the tier is set by the change, not the author, and some
// paths always need a person. Rules can only raise a unit's tier, never lower it.
package risk

import (
	"encoding/json"
	"os"
	"path"
	"strings"
)

// Tier says who should decide on a unit.
type Tier string

const (
	Static Tier = "static" // CI / linters are enough
	Agent  Tier = "agent"  // AI review is enough
	Human  Tier = "human"  // a person must decide
)

// Rank orders tiers so we can take the max.
func Rank(t Tier) int {
	switch t {
	case Human:
		return 2
	case Agent:
		return 1
	}
	return 0
}

// Rule maps a glob to a minimum tier.
type Rule struct {
	Glob   string `json:"glob"`
	Tier   Tier   `json:"tier"`
	Reason string `json:"reason"`
}

// Config is loaded from .godseye.json in the repo root (optional).
type Config struct {
	Rules []Rule `json:"rules"`
}

// Defaults cover the "always a human" paths from the article: auth, billing,
// CI workflows, release/installers, secrets, migrations.
var Defaults = []Rule{
	{".github/workflows/**", Human, "CI workflow"},
	{"**/*auth*/**", Human, "auth code"},
	{"**/*auth*.*", Human, "auth code"},
	{"**/*billing*/**", Human, "billing code"},
	{"**/*payment*/**", Human, "payment code"},
	{"**/*permission*.*", Human, "permissions"},
	{"**/*access*.*", Human, "access control"},
	{"**/migrations/**", Human, "database migration"},
	{"**/*.sql", Human, "database schema"},
	{"**/*secret*", Human, "secrets"},
	{"**/Dockerfile", Agent, "build image"},
	{"**/*.lock", Static, "lockfile"},
	{"**/package-lock.json", Static, "lockfile"},
	{"**/go.sum", Static, "lockfile"},
}

// Load reads .godseye.json if present and appends its rules to the defaults.
func Load(file string) []Rule {
	rules := append([]Rule{}, Defaults...)
	b, err := os.ReadFile(file)
	if err != nil {
		return rules
	}
	var c Config
	if json.Unmarshal(b, &c) == nil {
		rules = append(rules, c.Rules...)
	}
	return rules
}

// Floor returns the highest tier any rule demands for these paths, and why.
func Floor(rules []Rule, paths []string) (Tier, string) {
	best, why := Static, ""
	for _, p := range paths {
		for _, r := range rules {
			if Match(r.Glob, p) && Rank(r.Tier) > Rank(best) {
				best, why = r.Tier, r.Reason+" ("+p+")"
			}
		}
	}
	return best, why
}

// Match is a small glob matcher supporting **, * and ?.
func Match(glob, p string) bool {
	glob = strings.ToLower(glob)
	p = strings.ToLower(p)
	return matchParts(strings.Split(glob, "/"), strings.Split(p, "/"))
}

func matchParts(g, p []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			if len(g) == 1 {
				return true
			}
			for i := 0; i <= len(p); i++ {
				if matchParts(g[1:], p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, _ := path.Match(g[0], p[0]); !ok {
			return false
		}
		g, p = g[1:], p[1:]
	}
	return len(p) == 0
}
