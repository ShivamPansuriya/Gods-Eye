package review

import "encoding/json"

// Schema is the JSON schema sent as output_config.format, so the API returns
// exactly this shape. Kept hand-written so it matches types.go precisely and
// stays within what structured outputs supports (objects with
// additionalProperties:false, all keys required, enums).
func Schema() json.RawMessage { return json.RawMessage(schemaJSON) }

const schemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["title","eli5","verdict","units","not_checked","watch_after_deploy"],
  "properties": {
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
        "required": ["id","title","hunks","tier","risk","risk_reason","brief","flow","notes","checks","blast","ask"],
        "properties": {
          "id":          {"type": "string"},
          "title":       {"type": "string"},
          "hunks":       {"type": "array", "items": {"type": "string"}},
          "tier":        {"type": "string", "enum": ["human","agent","static"]},
          "risk":        {"type": "string", "enum": ["low","medium","high"]},
          "risk_reason": {"type": "string"},
          "brief":       {"type": "string"},
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
                "basis":  {"type": "string", "enum": ["seen_in_diff","inferred"]}
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
