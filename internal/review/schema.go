package review

import "encoding/json"

// MapSchema is the input schema of submit_review.
func MapSchema() map[string]any { return mustMap(mapSchemaJSON) }

// FindingsSchema is the input schema of submit_findings.
func FindingsSchema() map[string]any { return mustMap(findingsSchemaJSON) }

// VerdictSchema is the input schema of submit_verdict.
func VerdictSchema() map[string]any { return mustMap(verdictSchemaJSON) }

// Schema is kept for tests: the map schema as raw JSON.
func Schema() json.RawMessage { return json.RawMessage(mapSchemaJSON) }

func mustMap(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	// Tool schemas need Go []string for "required" at the top level.
	if req, ok := m["required"].([]any); ok {
		out := make([]string, len(req))
		for i, r := range req {
			out[i] = r.(string)
		}
		m["required"] = out
	}
	return m
}

const mapSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["story","title","eli5","verdict","units","not_checked","watch_after_deploy"],
  "properties": {
    "story": {
      "type": "object",
      "additionalProperties": false,
      "required": ["why","why_basis","what","before","after","impact","focus","review_mins"],
      "properties": {
        "why":       {"type": "string"},
        "why_basis": {"type": "string", "enum": ["stated","inferred"]},
        "what":      {"type": "string"},
        "before":    {"type": "string"},
        "after":     {"type": "string"},
        "focus":     {"type": "array", "items": {"type": "string"}},
        "review_mins": {"type": "integer"},
        "impact": {
          "type": "array",
          "items": {
            "type": "object",
            "additionalProperties": false,
            "required": ["who","area","change","kind"],
            "properties": {
              "who":    {"type": "string", "enum": ["users","admins","developers","operators","data"]},
              "area":   {"type": "string"},
              "change": {"type": "string"},
              "kind":   {"type": "string", "enum": ["added","changed","removed","risk"]}
            }
          }
        }
      }
    },
    "title":   {"type": "string"},
    "eli5":    {"type": "string"},
    "verdict": {"type": "string", "enum": ["approve","request_changes","needs_decision"]},
    "not_checked":        {"type": "array", "items": {"type": "string"}},
    "watch_after_deploy": {"type": "array", "items": {"type": "string"}},
    "units": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id","title","hunks","tier","risk","risk_reason","brief","question","flow","notes","checks","blast","ask"],
        "properties": {
          "id":          {"type": "string"},
          "title":       {"type": "string"},
          "hunks":       {"type": "array", "items": {"type": "string"}},
          "tier":        {"type": "string", "enum": ["human","agent","static"]},
          "risk":        {"type": "string", "enum": ["low","medium","high"]},
          "risk_reason": {"type": "string"},
          "brief":       {"type": "string"},
          "question":    {"type": "string"},
          "flow": {
            "type": "object",
            "additionalProperties": false,
            "required": ["before","after"],
            "properties": {
              "before": {"type": "array", "items": {"$ref": "#/$defs/step"}},
              "after":  {"type": "array", "items": {"$ref": "#/$defs/step"}}
            }
          },
          "notes": {
            "type": "array",
            "items": {
              "type": "object",
              "additionalProperties": false,
              "required": ["hunk","line","code","note"],
              "properties": {
                "hunk": {"type": "string"},
                "line": {"type": "integer"},
                "code": {"type": "string"},
                "note": {"type": "string"}
              }
            }
          },
          "checks": {
            "type": "array",
            "items": {
              "type": "object",
              "additionalProperties": false,
              "required": ["criterion","status","detail","source","evidence"],
              "properties": {
                "criterion": {"type": "string"},
                "status":    {"type": "string", "enum": ["done","not_done","no_evidence"]},
                "detail":    {"type": "string"},
                "source":    {"type": "string", "enum": ["pr","issue","guidance","inferred"]},
                "evidence": {
                  "type": "object",
                  "additionalProperties": false,
                  "required": ["kind","hunk","line","ref"],
                  "properties": {
                    "kind": {"type": "string", "enum": ["code","test","doc","gap"]},
                    "hunk": {"type": "string"},
                    "line": {"type": "integer"},
                    "ref":  {"type": "string"}
                  }
                }
              }
            }
          },
          "blast": {
            "type": "array",
            "items": {
              "type": "object",
              "additionalProperties": false,
              "required": ["title","detail","basis"],
              "properties": {
                "title":  {"type": "string"},
                "detail": {"type": "string"},
                "basis":  {"type": "string", "enum": ["seen_in_diff","seen_in_repo","inferred"]}
              }
            }
          },
          "ask": {"type": "array", "items": {"type": "string"}}
        }
      }
    }
  },
  "$defs": {
    "step": {
      "type": "object",
      "additionalProperties": false,
      "required": ["label","edge","state","hunk","line"],
      "properties": {
        "label": {"type": "string"},
        "edge":  {"type": "string"},
        "state": {"type": "string", "enum": ["same","added","removed","changed"]},
        "hunk":  {"type": "string"},
        "line":  {"type": "integer"}
      }
    }
  }
}`

const findingsSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["findings"],
  "properties": {
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["title","file","line","quote","severity","category","scenario","fix","bug_class","also_at","confidence"],
        "properties": {
          "title":     {"type": "string"},
          "file":      {"type": "string"},
          "line":      {"type": "integer"},
          "quote":     {"type": "string"},
          "severity":  {"type": "string", "enum": ["important","minor","nit"]},
          "category":  {"type": "string", "enum": ["logic","data","concurrency","security","contract","error_handling","resource","performance","other"]},
          "scenario":  {"type": "string"},
          "fix":       {"type": "string"},
          "bug_class": {"type": "string"},
          "confidence":{"type": "string", "enum": ["low","medium","high"]},
          "also_at": {
            "type": "array",
            "items": {
              "type": "object",
              "additionalProperties": false,
              "required": ["file","line"],
              "properties": {"file": {"type": "string"}, "line": {"type": "integer"}}
            }
          }
        }
      }
    }
  }
}`

const verdictSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["real","introduced","severity","confidence","reason"],
  "properties": {
    "real":       {"type": "boolean"},
    "introduced": {"type": "boolean"},
    "severity":   {"type": "string", "enum": ["important","minor","nit"]},
    "confidence": {"type": "string", "enum": ["low","medium","high"]},
    "reason":     {"type": "string"}
  }
}`
