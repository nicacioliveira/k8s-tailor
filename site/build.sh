#!/usr/bin/env bash
# Assembles the public site into _site/: the landing page (site/), the documentation (MkDocs,
# _site/docs/) and the files they share. Used by .github/workflows/pages.yml and for local previews:
#   python3 -m venv .venv && .venv/bin/pip install -r site/requirements.txt && PATH=.venv/bin:$PATH site/build.sh
set -euo pipefail
cd "$(dirname "$0")/.."
rm -rf _site
NO_MKDOCS_2_WARNING=1 mkdocs build --strict
cp site/index.html site/style.css _site/
mkdir -p _site/assets/fonts _site/assets/img
cp site/assets/favicon.svg _site/assets/
cp docs/assets/fonts/InterVariable-latin.woff2 docs/assets/fonts/Inter-OFL.txt _site/assets/fonts/
for f in workload-reasoning workloads policy-containers; do cp "docs/images/$f.png" _site/assets/img/; done
cp llms.txt llms-full.txt _site/ 2>/dev/null || true
cp site/404.html _site/404.html
touch _site/.nojekyll
echo "site built in _site/ ($(du -sh _site | cut -f1))"
