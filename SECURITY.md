# Reporting a vulnerability

If you find a way out of a cage, a way to reach something a cage shouldn't, or a way to read a secret zoo is meant to keep from an agent, please report it privately through [GitHub's private vulnerability reporting](https://github.com/dustywusty/zoo/security/advisories/new) rather than a public issue.

Helpful to include: the backend (`zoo backend`), zoo version (`zoo version`), what you did from inside the cage, and what you could reach or read. `zoo doctor --json` output helps too.

What zoo does and doesn't protect against is in [docs/security.md](docs/security.md). Things listed there under known gaps are already known; better ways to close them are very welcome.
