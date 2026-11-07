package scope

// The built-in path lists below are taken from alibaba/open-code-review
// (internal/config/allowlist, Apache-2.0; see NOTICE) and sorted by why a file
// is not hunted, so the report can say which reason applied.

// secretPatterns are credential files. Their contents never reach a model:
// they are dropped from the diff and every tool refuses to read them.
var secretPatterns = []string{
	"**/.ssh/**", "**/id_rsa", "**/id_dsa", "**/id_ecdsa", "**/id_ed25519",
	"**/.netrc", "**/_netrc", "**/.npmrc", "**/.pypirc", "**/.dockercfg",
	"**/*.pem", "**/*.key", "**/*.p12", "**/*.pfx", "**/*.keystore", "**/*.jks",
	"**/.git-credentials", "**/*.env", "**/.envrc", "**/.aws/credentials", "**/.htpasswd", "**/*.tfvars", "**/*.kdbx",
}

// testPatterns are tests. They are hunted by the tests lens only.
var testPatterns = []string{
	"**/*_test.go", "**/src/test/java/**/*.java", "**/src/test/**/*.{kt,kts}", "**/*Test.fs",
	"**/*.test.{js,jsx,ts,tsx}", "**/*.spec.{js,jsx,ts,tsx}", "**/__tests__/**",
	"**/test/**/*_test.py", "**/tests/**/*_test.py", "**/*_test.py", "**/test_*.py",
	"**/*_spec.rb", "**/spec/**/*_spec.rb", "**/*Test.java", "**/*Tests.java", "**/*_test.rs",
	"**/*.test.ets", "**/test/**/*.jl", "**/test/**/*.hs", "**/*Spec.hs", "**/test/**/*.lhs", "**/*Spec.lhs",
	"**/tests/**/*.nim", "**/tests/**/*.nu", "**/test/**/*.nu", "**/*_test.nu", "**/test_*.nu", "**/test-*.nu",
	"**/tests/**/*.R", "**/*Test.swift", "**/*Tests.swift", "**/Tests/**/*.swift", "**/tests/**/*.elm",
	"**/test/**/*.zig", "**/*_test.zig", "**/test/**/*.ml", "**/tb_*.{v,sv,vhd,vhdl}", "**/*_tb.{v,sv,vhd,vhdl}",
	"**/*.t.sol", "**/test/**/*.sol", "**/tests/**/*.sol", "**/test/**/*.vy", "**/tests/**/*.vy",
}

// skipPatterns are never hunted; each group carries the reason shown in the report.
var skipPatterns = []struct {
	Reason   string
	Patterns []string
}{
	{"test data", []string{"**/__snapshots__/**", "**/*.snap", "**/testdata/**", "**/fixtures/**"}},
	{"generated", []string{
		"**/*.generated.*", "**/*.gen.go", "**/*.pb.go", "**/*.pb.cc", "**/*.pb.h", "**/kitex_gen/**/*.go",
		"**/*.capnp.h", "**/*.capnp.go", "**/*.capnp.ts", "**/*_capnp.rs", "**/*_capnp.py",
		"**/*.min.{js,css}", "**/generated/**",
	}},
	{"lockfile", []string{
		"**/package-lock.json", "**/pnpm-lock.yaml", "**/npm-shrinkwrap.json", "**/yarn.lock", "**/go.sum",
		"**/Cargo.lock", "**/poetry.lock", "**/Pipfile.lock", "**/composer.lock", "**/Gemfile.lock",
		"**/.terraform.lock.hcl", "**/*.lock",
	}},
	{"dependency", []string{
		"**/node_modules/**", "**/bower_components/**", "**/.pnpm-store/**",
		"**/.yarn/{cache,unplugged,releases,sdks,patches}/**", "**/.pnp.cjs", "**/.pnp.loader.mjs",
		"**/vendor/**", "**/.bundle/**", "**/oh_modules/**", "**/site-packages/**", "**/.venv/**", "**/venv/**",
		"**/Pods/**", "**/Carthage/**", "lib/**/*.sol",
	}},
	{"build output", []string{
		"**/dist/**", "**/.next/**", "**/.nuxt/**", "**/.svelte-kit/**", "**/.astro/**", "**/.turbo/**",
		"**/.angular/**", "**/.parcel-cache/**", "**/.docusaurus/**", "**/target/**", "**/.gradle/**",
		"**/__pycache__/**", "**/*.egg-info/**", "**/.tox/**", "**/.mypy_cache/**", "**/.pytest_cache/**",
		"**/.ruff_cache/**", "**/.build/**", "**/obj/**", "**/.dart_tool/**", "**/.terraform/**",
		"**/.stack-work/**", "**/coverage/lcov-report/**", "**/.ipynb_checkpoints/**",
	}},
}

// codeExts are the extensions worth hunting; a file with any other extension
// (docs, images, data) is not code. Files without an extension (Dockerfile,
// Makefile) are kept.
var codeExts = map[string]bool{}

func init() {
	for _, e := range []string{
		".java", ".kt", ".kts", ".scala", ".groovy", ".py", ".pyi", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs",
		".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".hxx", ".cs", ".vb", ".fs", ".fsi", ".fsx", ".go", ".rs",
		".rb", ".rake", ".gemspec", ".php", ".phtml", ".swift", ".m", ".mm", ".sh", ".bash", ".zsh", ".fish",
		".ps1", ".psm1", ".psd1", ".sql", ".css", ".scss", ".sass", ".less", ".html", ".htm", ".ftl", ".ftlh",
		".ftlx", ".hbs", ".mustache", ".jinja2", ".j2", ".pug", ".haml", ".slim", ".erb", ".astro", ".vue",
		".ipynb", ".svelte", ".xml", ".yaml", ".yml", ".json", ".toml", ".ini", ".env", ".gradle", ".cmake",
		".r", ".lua", ".pl", ".pm", ".ex", ".exs", ".erl", ".hrl", ".ets", ".json5", ".jsonc", ".dart", ".tf",
		".tofu", ".graphql", ".gql", ".prisma", ".jl", ".hcl", ".tfvars", ".bicep", ".proto", ".nix", ".hs",
		".lhs", ".nim", ".nims", ".nimble", ".nu", ".elm", ".properties", ".po", ".pot", ".jsonnet",
		".libsonnet", ".zig", ".thrift", ".capnp", ".bzl", ".bazel", ".star", ".ml", ".mli", ".re", ".rei",
		".v", ".sv", ".vh", ".vhd", ".vhdl", ".sol", ".vy", ".rego", ".glsl", ".hlsl", ".wgsl", ".metal",
		".ejs", ".liquid", ".njk",
	} {
		codeExts[e] = true
	}
}
