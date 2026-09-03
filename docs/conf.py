# Configuration file for the Sphinx documentation builder.
#
# For the full list of built-in configuration values, see the documentation:
# https://www.sphinx-doc.org/en/master/usage/configuration.html

# -- Path setup --------------------------------------------------------------

import subprocess
from datetime import datetime
from pathlib import Path

# -- Project information -----------------------------------------------------

project = 'billet'
author = 'junioryono'
copyright = f'{datetime.now().year}, {author}'

# Read the latest release version from git tags.
def get_version():
    try:
        result = subprocess.run(
            ['git', 'describe', '--tags', '--match', 'v[0-9]*', '--abbrev=0'],
            capture_output=True,
            text=True,
            cwd=Path(__file__).resolve().parent.parent,
            check=False,
        )
        if result.returncode == 0:
            return result.stdout.strip()
    except Exception:
        pass
    return 'v0.0.0'

# The full version, including alpha/beta/rc tags
release = get_version()
version = release

# -- General configuration ---------------------------------------------------

# Add any Sphinx extension module names here, as strings.
extensions = [
    'myst_parser',
    'sphinx_rtd_theme',
    'sphinxext.rediraffe',
    'sphinx_copybutton',
]

# Add any paths that contain templates here, relative to this directory.
templates_path = ['_templates']

# List of patterns, relative to source directory, that match files and
# directories to ignore when looking for source files.
#
# The Markdown files at the top of docs/ are the operator guides and ADRs that
# the code and CLAUDE.md link to by path; they are not Sphinx sources. Sphinx
# content lives in subdirectories, and `*.md` does not cross a directory
# boundary, so only the top-level files are excluded.
exclude_patterns = ['_build', '_venv', 'Thumbs.db', '.DS_Store', '*.md']

# -- Options for HTML output -------------------------------------------------

# The theme to use for HTML and HTML Help pages.
html_theme = 'sphinx_rtd_theme'

# Add any paths that contain custom static files (such as style sheets) here,
# relative to this directory.
html_static_path = ['_static']

html_theme_options = {
    'prev_next_buttons_location': 'both',
    'style_external_links': False,
    'style_nav_header_background': '#2980B9',
    # Toc options
    'collapse_navigation': False,
    'sticky_navigation': True,
    'navigation_depth': 4,
    'includehidden': True,
    'titles_only': False
}

html_context = {
    'display_github': True,
    'github_user': 'junioryono',
    'github_repo': 'billet',
    'github_version': 'main',
    'conf_py_path': '/docs/',
}

def setup(app):
    app.add_css_file('customize.css')

# MyST parser configuration
myst_enable_extensions = [
    "attrs_inline",
    "colon_fence",
    "deflist",
    "tasklist",
]

# Copy button configuration
copybutton_prompt_text = r"$ |>>> |\.\.\. "
copybutton_prompt_is_regexp = True

# Rediraffe configuration for redirects: a page that moves gets an entry here so
# its old URL keeps resolving.
rediraffe_redirects = {}
