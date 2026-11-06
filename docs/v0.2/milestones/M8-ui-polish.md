# M8 · UI polish, then release

Make it feel finished, then ship v0.1.0 and v0.2.0 together (Q8, Q55).

### M8-01 · UX pass over every screen
**Status:** todo
**Depends on:** M5-04

Walk every screen with real Agents: empty states, loading, errors, offline, locked, queued; wording consistent with
the glossary; no dead ends (every error says what to do).

**Done when:** a list of findings is fixed and before/after screenshots are attached to the PR.

### M8-02 · Accessibility and phones
**Status:** todo
**Depends on:** M8-01

Keyboard navigation, focus, contrast in both themes, screen-reader labels; every screen usable on a phone.

**Done when:** an automated accessibility check passes and a phone walk-through is recorded.

### M8-03 · Performance
**Status:** todo
**Depends on:** M8-01

Fast first load (bundle sizes, lazy loading), smooth live updates with many apps and Hosts (hundreds), no needless
re-renders.

**Done when:** numbers before/after are recorded (load size, time to interactive, update latency with 200 apps).

### M8-04 · Visual consistency
**Status:** todo
**Depends on:** M8-01

Badges, colors, spacing and animations consistent; Catppuccin in all flavors where shown; new README screenshots.

**Done when:** screenshots replaced in the README.

### M8-05 · Release notes
**Status:** todo
**Depends on:** M7-05

Release notes for v0.2.0 (what's new, what's removed, reinstall from v0.1) and v0.1.0 (what it is, that it's the push
line with fixes only).

**Done when:** reviewed.

### M8-06 · Release v0.1.0 and v0.2.0
**Status:** todo
**Depends on:** M8-05, M7-04

Tag `v0.1.0` on `release-0.1` and `v0.2.0` on `main`; images public; GitHub releases with the notes.

**Done when:** both releases are published and installable with `install.sh`.
