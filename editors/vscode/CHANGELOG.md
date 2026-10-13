# Changelog

All notable changes to the Gocpy extension are documented in this file.

## 0.3.0

- English user interface: command titles, settings, notifications, snippet
  names and semantic token descriptions.
- New README with screenshots, settings, commands and troubleshooting.
- Completion items show the declaration and its documentation comment,
  including documentation of Go functions (requires gocpy 0.4.1 or newer).

## 0.2.1

- Re-analyze when `.go` files change. Go files of the package directory can be
  imported from Gocpy with `import "./file.go"` (gocpy 0.4.0 or newer).
- Import path completion suggests the Go files of the package directory.
  Go to Definition and hover lead into these files.

## 0.2.0

- Language server support through `gocpy lsp` (requires gocpy 0.3.0 or newer):
  - diagnostics while typing;
  - Go to Definition, including Go sources;
  - Find All References and Rename across packages;
  - Call Hierarchy and hover documentation;
  - completion of keywords, annotations, package and type members and
    import paths;
  - signature help and document and workspace symbols.
- Semantic highlighting with the token classes of the Gocpy IDE.
- The Gocpy Dark theme with the colors of the Gocpy IDE.
- A new grammar: keyword groups, f-strings, number literals, annotations,
  intrinsics, `TODO` markers.
- Background highlighting of `unsafe` blocks, snippets, and run and build
  commands.

## 0.1.0

- Initial release: syntax highlighting, the `.gocpy` file icon and language
  configuration.
