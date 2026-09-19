# Attribution and modification notice

This is an unofficial source extension for Wei-Shaw/sub2api.

- Upstream: https://github.com/Wei-Shaw/sub2api
- Baseline: v0.2.6, commit `49a39b6dc1abed30fd227611e8af1108bc427610`.
- License: GNU Lesser General Public License v3; see LICENSE and the incorporated GNU GPL v3 text in COPYING.GPL3. Existing upstream copyright notices remain applicable.
- Modifications dated 2026-09-18: per-account STATE controls, manual Pro/Team selection, global harvest pool integration, fixed-business-proxy verification, retention and renewal handling, response watchdog, admin UI, tests and documentation.
- Modifications dated 2026-09-19 (fork by zhoushun98): per-account multi-model tickets (gpt-6-astra and gpt-5.6-sol each with its own ticket, strict per-model gating), and global defaults that auto-enable STATE for newly created accounts. Patches are kept under docs/patches/.
- The exact modified and added paths, baseline file hashes and overlay hashes are recorded in UPSTREAM.json. A null baseline hash denotes an added file.

Design reference: https://github.com/gylive/ccodex-sleep-state at commit `26b22196bf68b372d0daad9381f686a3321068d4`, specifically ticket lifecycle and status presentation ideas. No source code or dependencies from that project are included in this extension. This implementation uses Sub2API account persistence and fixed-business-proxy verification.

Thanks to both upstream projects and community members for discussion and feedback. No affiliation or endorsement is implied.
- Modifications dated 2026-09-19 (fork by zhoushun98): rebased the overlay onto upstream v0.2.7 (aea725f2), carrying forward upstream PR #7315 which was dropped from upstream main; merged wangyunjeff's commit 94068e5 (direct-route STATE verification); harvest retry widened to 30 attempts with a one-minute cooldown.
