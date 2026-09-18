"""Point the Homebrew formula at a released source tarball.

Called from .github/workflows/release.yml with the tarball URL and its
checksum. The formula starts out head-only, so the first release has to insert
a stable spec; later ones just rewrite it.
"""

import re
import sys

FORMULA = "Formula/minitail.rb"


def bump(src: str, url: str, sha: str) -> str:
    if re.search(r'^  url ".*"$', src, re.M):
        src = re.sub(r'^  url ".*"$', lambda _: f'  url "{url}"', src, count=1, flags=re.M)
        return re.sub(r'^  sha256 ".*"$', lambda _: f'  sha256 "{sha}"', src, count=1, flags=re.M)

    # First tagged release: a stable spec goes directly after the homepage.
    src, n = re.subn(
        r'^(  homepage ".*"\n)',
        lambda m: m.group(1) + f'  url "{url}"\n  sha256 "{sha}"\n',
        src,
        count=1,
        flags=re.M,
    )
    if n != 1:
        raise SystemExit(f"{FORMULA}: could not find the homepage line to insert after")
    return src


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit("usage: bump-formula.py <url> <sha256>")
    url, sha = sys.argv[1], sys.argv[2]
    with open(FORMULA) as f:
        src = f.read()
    with open(FORMULA, "w") as f:
        f.write(bump(src, url, sha))


if __name__ == "__main__":
    main()
