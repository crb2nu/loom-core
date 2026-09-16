Resolve ranged version specifiers to a single concrete version when deriving
devbox base-image tags. `parsePythonVersion` character-trimmed the
`requires-python` value from pyproject.toml, so a ranged specifier like
`>=3.11,<3.14` (libs/py-sprite-kit) became `3.11,<3.14` and was interpolated
verbatim into the Dockerfile, producing the invalid image reference
`python:3.11,<3.14-slim-bookworm` and failing every sandbox build for such
repos. The detector now parses `requires-python` as a PEP 440 specifier set
(`>=`, `>`, `<`, `<=`, `==`, `!=`, `~=`, `.*` wildcards) and selects the
lowest major.minor series satisfying every clause. The Node path had the same
defect class for compound `engines.node` semver ranges (`>=18 <21` leaked
`18 <21` into the tag) and now resolves the range floor instead; the Go path
was already safe because the go.mod directive is a single concrete version,
but it now guards against trailing comments leaking into the tag.
