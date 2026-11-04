# 👁 God's Eye

**PR review you can read in two minutes.**

Agents write more code than people can read line by line. God's Eye doesn't try to read *for* you. It tells you **what changed, whether it did what it claims, and which parts need your decision**, in as few words as possible.

![Guided view](docs/guided.png)

## What it does

1. **Splits the PR into units.** A unit is one *behaviour* change ("Index writes retry when the DB is busy"), not a file.
2. **Sorts units by who must decide:**
   - **Needs your decision:** security, data, money, concurrency, intent conflicts, or anything that failed a check
   - **Agent reviewed:** real logic, but the evidence covers it
   - **Static / CI:** no behaviour change
3. **Checks the "done when" criteria** from the PR, linked issues and `REVIEW.md` / `CLAUDE.md`. Each one is marked **Done**, **Not done** or **No evidence**, with a link to the line that proves it.
4. **Shows each unit at four depths:**

| View | What you get |
|---|---|
| **Facts** | Files, line counts, touched functions, check tally and the diff. No prose. |
| **Briefed** | One short paragraph with the claims to verify underlined, plus a before → after flow. |
| **Explained** | The flow, plus notes on the 2-4 lines that carry the change, pinned in the diff. |
| **Guided** | Done-when checklist with evidence, blast radius, and a ready-to-send change request. |

No nitpicks, no style comments, no "consider…" ideas. The model returns strict JSON with word limits on every field, and the CLI trims anything that runs over.

## Install

```bash
go install github.com/ShivamPansuriya/gods-eye@latest
```

The binary is `gods-eye`. Rename or alias it to `godseye` if you like. Needs [`gh`](https://cli.github.com) for PR mode and an Anthropic API key:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
```

## Use

```bash
gods-eye 123                        # review PR #123 in the current repo
gods-eye https://github.com/o/r/pull/123
gods-eye 123 --repo owner/name
gods-eye --local --base main        # current branch vs main, no GitHub needed
gods-eye 123 --comment              # also post a short summary on the PR
gods-eye 123 --effort medium        # cheaper / faster
gods-eye 123 --save r.json          # keep the raw review; re-render with --from r.json
```

It writes a single offline HTML file to `.godseye/pr-123.html` and opens it.

**Keys:** `j` / `k` move between units, `1`-`4` switch views. Click any flow step, note, or evidence chip to jump to that line. The **Approve** and **Request changes** buttons copy the matching `gh pr review` command.

## Risk rules

Path rules can only **raise** a unit's tier, never lower it. Built in: CI workflows, auth, billing, payments, permissions, migrations, and secrets always need a person. Add your own in `.godseye.json` at the repo root:

```json
{
  "rules": [
    { "glob": "src/updater/**", "tier": "human", "reason": "auto-updater" },
    { "glob": "docs/**",        "tier": "static", "reason": "docs" }
  ]
}
```

Tell the reviewer what matters in your code with a `REVIEW.md`. It's read on every run, along with `CLAUDE.md` and `AGENTS.md`.

## Run on every PR (GitHub Action)

See [`.github/workflows/godseye.yml`](.github/workflows/godseye.yml). It posts the summary comment and uploads the full page as an artifact. Add `ANTHROPIC_API_KEY` to the repo's secrets.

## Try it without an API key

```bash
go run . --diff examples/retry.diff --review-file examples/retry.review.json
```

## How it works

```
gh pr view / gh pr diff ──► diff parser (hunks H1..Hn, line numbers)
            │                        │
  PR body, linked issues,            ▼
  REVIEW.md, CLAUDE.md ──► Claude (structured JSON output, word limits)
                                     │
                                     ▼
              Finalize: every hunk in exactly one unit, path rules raise tiers,
              a failing check forces "human", invalid line refs are dropped,
              long text is trimmed
                                     │
                     ┌───────────────┴───────────────┐
                     ▼                               ▼
          single-file HTML page              Markdown PR comment
```

Default model: `claude-opus-5-5` at `high` effort. A typical 300-line PR costs a few cents. The CLI prints the exact token count and cost after each run.

## Roadmap

- Real caller lookup for blast radius (today it's marked *inferred* when it isn't visible in the diff)
- Intent-conflict detection against past PRs and decisions
- Incident-based eval set: score the reviewer on PRs that caused real bugs
- Re-review only the new diff, with a cap on review rounds
