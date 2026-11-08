# Finding Applications by Host and App of Apps

For the grilling before it is planned (after v0.2.0; [ROADMAP](../../ROADMAP.md)). Written 2026-11-08 from the idea
"click a Host, see its Applications". Expand it here; decisions go to an ADR or the next version's decisions.md.

## Why

With a few Hosts and tens of Applications, the questions people ask the list are "what runs on prod-1?", "what is
broken on the Host that just went offline?" and "what belongs to this App of Apps?". Today the text search answers
them only by accident (it matches the Host name inside other fields too) and nothing on a Host leads to its apps.

## What exists (v0.2)

- Applications list: text search over name, Host, repository, path, statuses; Sync Status and Health filters (OR
  within one, AND across); Child Applications grouped under their App of Apps, collapsible; filtering expands groups.
- Host cards (M5-02): name, online state, engine, Agent version, number of apps (not clickable), key actions.
- Badges (M5-03): Host offline, engine, queued Sync, rolled back, on app tiles and the app header.
- No filter state in the URL: a reload or a shared link loses it.

## Proposal

1. **Host filter and App of Apps filter** beside Sync and Health, same rules, each option with a live count
   ("prod-1 · 12") and, for Hosts, the online dot.
2. **Everything that names a Host leads to its apps**: the app count on a Host card, the Host badge on a tile, the Host
   in an app's header. Same for an App of Apps: its name anywhere leads to its children.
3. **Filters in the URL** (`/apps?host=prod-1&sync=OutOfSync`): back, reload, bookmarks and shared links keep them. A
   bookmark is the saved view; no saved-views feature.
4. **Group by** App of Apps (today) or Host, each group with a health summary ("prod-1: 11 Healthy, 1 Degraded") and,
   for Hosts, online or last seen.
5. Maybe **search tokens** (`host:prod-1 sync:outofsync`) and `/` to focus the search.

## Open questions

1. **Where does a Host click go?** (a) the Applications list with the Host filter set; (b) a Host page: its card on
   top, its apps below, a home for Host actions; (c) both, the Host page's list being the filtered list.
   *Leaning (a)*: one list with one set of tools; a Host page only if Host actions outgrow the cards.
2. **Grouping**: a switch between App of Apps and Host; always Host first with the App of Apps trees inside; or App
   of Apps only. An App of Apps whose children run on several Hosts: shown under each Host (with only the children
   there), or once under "several Hosts"? *Leaning: a switch; under each Host, children filtered.*
3. **App of Apps filter**: needed as a filter, or is clicking it in the tree (collapse the rest) enough? Does it
   include grandchildren?
4. **Which extras**: URL state (*leaning yes*, cheap), counts (*yes* if they stay live over SSE without cost), search
   tokens (later?), keyboard shortcuts.
5. **Offline Hosts**: does filtering by an offline Host show last-known state (M4-02) with the offline badge, and
   does the group summary say "offline since …" instead of health?
6. **Many Hosts**: from what number does the Host filter need its own search (a combobox instead of chips)?
7. **Phones** (M8-02): where do the filters go on a narrow screen; does grouping by Host replace the tree there?
8. **Words**: "Host" and "App of Apps" as in the [glossary](../../GLOSSARY.md); does "group" need a term, or is it
   just the list's view?

## To check before deciding

- The list's cost with a few hundred Applications and live counts over SSE (M8-03 measures the list in v0.2).
- How the UI test suite covers filters today, so the new ones get the same tests (UI unit tests for the logic, an e2e
  through the API for the URL state).
- That the Host filter works for Applications whose Host was renamed or removed (Q37, Q53).
