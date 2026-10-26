"""MkDocs hooks for the documentation site.

1. Adds the root-level project pages (CHANGELOG, SECURITY, CONTRIBUTING, CODE_OF_CONDUCT)
   to the site, so they sit next to docs/ in the navigation.
2. Rewrites relative links that leave docs/ (dev/calico/README.md, deploy/argocd/...,
   ../README.md, LICENSE) to the matching GitHub URL, and links between the included root
   pages and docs/ to site-relative paths, so the Markdown stays valid on GitHub AND here.
"""
import os
import re

from mkdocs.structure.files import File

REPO = "https://github.com/nicacioliveira/k8s-tailor"
BRANCH = "main"
ROOT_PAGES = ["CHANGELOG.md", "SECURITY.md", "CONTRIBUTING.md", "CODE_OF_CONDUCT.md"]

LINK = re.compile(r"(?<!\!)\]\(([^)\s]+)(\s+\"[^\"]*\")?\)")
FENCE = re.compile(r"```.*?```", re.S)


def on_files(files, config):
    root = os.path.dirname(os.path.abspath(config["config_file_path"]))
    for name in ROOT_PAGES:
        if os.path.exists(os.path.join(root, name)):
            files.append(File(name, root, config["site_dir"], config["use_directory_urls"]))
    return files


def _repo_path(page):
    """Path of the page's source file inside the repository."""
    src = page.file.src_uri
    return src if src in ROOT_PAGES else "docs/" + src


def _site_path(repo_path):
    """Path inside docs_dir of a repository file that is part of the site, else None."""
    if repo_path.startswith("docs/") and repo_path.endswith(".md"):
        return repo_path[len("docs/"):]
    if repo_path in ROOT_PAGES:
        return repo_path
    if repo_path.startswith("docs/images/") or repo_path.startswith("docs/assets/"):
        return repo_path[len("docs/"):]
    return None


def on_page_markdown(markdown, page, config, files):
    root = os.path.dirname(os.path.abspath(config["config_file_path"]))
    here = os.path.dirname(_repo_path(page))
    if page.file.src_uri in ROOT_PAGES:
        page.edit_url = None  # the edit URI of docs/ would point to the wrong file

    def fix(m):
        link, title = m.group(1), m.group(2) or ""
        if re.match(r"^([a-z][a-z0-9+.-]*:|#|/)", link):
            return m.group(0)
        path, _, frag = link.partition("#")
        if not path:
            return m.group(0)
        target = os.path.normpath(os.path.join(here, path)).replace(os.sep, "/")
        site = _site_path(target)
        suffix = ("#" + frag) if frag else ""
        if site is not None:
            cur = os.path.dirname(page.file.src_uri) or "."
            return "](" + os.path.relpath(site, cur).replace(os.sep, "/") + suffix + title + ")"
        if target.startswith(".."):
            return m.group(0)
        kind = "tree" if os.path.isdir(os.path.join(root, target)) else "blob"
        return "](" + f"{REPO}/{kind}/{BRANCH}/{target}" + suffix + title + ")"

    out, pos = [], 0
    for fence in FENCE.finditer(markdown):
        out.append(LINK.sub(fix, markdown[pos:fence.start()]))
        out.append(fence.group(0))
        pos = fence.end()
    out.append(LINK.sub(fix, markdown[pos:]))
    return "".join(out)
