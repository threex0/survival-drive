# Vendored licence texts

Canonical licence texts shipped into `LICENSES/` on every image, named after the
`license` id used in `profiles/catalogue.json` (lower-cased, `.` → `-`).

| File | Licence id | Source |
|---|---|---|
| `cc-by-sa-4-0.txt` | `CC-BY-SA-4.0` | creativecommons.org/licenses/by-sa/4.0/legalcode.txt |
| `cc-by-nc-sa-3-0.txt` | `CC-BY-NC-SA-3.0` | creativecommons.org/licenses/by-nc-sa/3.0/legalcode.txt |
| `cc-by-nc-nd-4-0.txt` | `CC-BY-NC-ND-4.0` | creativecommons.org/licenses/by-nc-nd/4.0/legalcode.txt |
| `gpl-3-0-or-later.txt` | `GPL-3.0-or-later` | gnu.org/licenses/gpl-3.0.txt |
| `mit.txt` | `MIT` | SPDX licence-list-data |
| `apache-2-0.txt` | `Apache-2.0` | apache.org/licenses/LICENSE-2.0.txt |
| `odbl-1-0.txt` | `ODbL-1.0` | SPDX licence-list-data |
| `public-domain-us.txt` | `Public-Domain-US` | project text (below) |
| `public-domain-us-gov.txt` | `Public-Domain-US-Gov` | project text (below) |

The two `public-domain-*.txt` files are project-written notices, not legal codes.

## Missing

| Licence id | Why | Action |
|---|---|---|
| `Gemma-Terms-of-Use` | Google publishes HTML at <https://ai.google.dev/gemma/terms> | Vendor verbatim before selling any image containing the Gemma models |
| `Unverified` | zimgit micro-packs; upstream licence unconfirmed | Excluded from `sell`; verify before use |

`image build` reports every missing text and still writes `LICENSES/NOTICE.md`
with attribution for all content, so the gap is visible rather than silent.
