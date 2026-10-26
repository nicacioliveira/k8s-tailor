# Public site

`site/` is the landing page (plain HTML and CSS, no framework); the documentation is the Markdown in
[`docs/`](../docs), built with MkDocs Material from [`mkdocs.yml`](../mkdocs.yml). `site/build.sh` assembles both
into `_site/` (landing at the root, docs under `/docs/`), and `.github/workflows/pages.yml` publishes it to GitHub Pages.

Preview locally:

```bash
python3 -m venv .venv && .venv/bin/pip install -r site/requirements.txt
PATH=.venv/bin:$PATH site/build.sh && python3 -m http.server 8000 -d _site
```

Notes:

- `site/mkdocs_hooks.py` adds CHANGELOG, SECURITY, CONTRIBUTING and CODE_OF_CONDUCT to the docs nav and turns relative links
  that leave `docs/` (for example `dev/calico/README.md`) into GitHub URLs, so the Markdown stays valid on GitHub and on the site.
- The build is `mkdocs build --strict`: a broken link or anchor fails it.
- Inter is self-hosted (`docs/assets/fonts`, SIL OFL 1.1, see the repository `NOTICE`). Mermaid diagrams are rendered in the browser by a script
  that Material for MkDocs loads from unpkg.com; everything else is served from the site itself.
- The screenshots use the dashboard's synthetic fixture data, as [docs/SCREENS.md](../docs/SCREENS.md) says.
