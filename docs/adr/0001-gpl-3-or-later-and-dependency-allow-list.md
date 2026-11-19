# GPL-3.0-or-later, with a GPL-3-compatible dependency allow-list

runner-microvm is licensed GPL-3.0-or-later: derivatives must stay open source, and it matches forgejo-runner, so code can move between the two projects in either direction. Every dependency must carry a GPL-3-compatible license, checked in CI. The allow-list is MIT, BSD, Apache-2.0, MPL-2.0, LGPL, GPL-3, ISC, 0BSD, Unlicense and CC0-1.0: all are GPL-3-compatible, and ISC is common in Go dependency trees. GPL-2.0-only is excluded, because it can't be combined with GPL-3.

## Consequences

- This is hard to reverse: relicensing needs the consent of every contributor.
- It is a deliberate deviation: most Go projects are MIT or Apache-2.0, so a reader may expect a permissive license here.
- A dependency under any license outside the allow-list fails the build. The check itself is described in [`docs/conventions.md`](../conventions.md#dependency-licenses).
