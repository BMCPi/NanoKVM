# UI Design Guide

Component selection reference, current-state audit, and redesign blueprint for the NanoKVM BMC web UI.

**Stack:** Go + [templ](https://templ.guide) + HTMX v2, with [shadcn-templ](https://shadcn-templ.com)
(templUI 2.0) vendored under `ui/components/`. There is no Datastar in this app — dynamic state is
HTMX fragments plus per-component vanilla IIFE scripts, SSE, and WebSockets.

**Provenance:** compiled 2026-08-18 from a five-lane research pass (shadcn-templ docs, codebase
inventory, live UI capture of `10.42.0.19`, JetKVM source analysis, peer/UX literature). Every
load-bearing claim was independently re-verified against its primary source; see
[Appendix: method](#appendix-method-and-confidence). File references are `path:line` at branch `edk2`.

---

## Contents

1. [Executive summary](#1-executive-summary)
2. [Component selection guide](#2-component-selection-guide)
3. [Current component usage](#3-current-component-usage)
4. [Conventions to preserve](#4-conventions-to-preserve)
5. [Current UI assessment](#5-current-ui-assessment)
6. [JetKVM comparison](#6-jetkvm-comparison)
7. [Peer patterns and design guidance](#7-peer-patterns-and-design-guidance)
8. [Redesign blueprint](#8-redesign-blueprint)
9. [Roadmap and cleanup backlog](#9-roadmap-and-cleanup-backlog)
10. [Appendix: method and confidence](#appendix-method-and-confidence)

---

## 1. Executive summary

| Metric | Value |
| --- | --- |
| shadcn-templ catalog size | 52 components (26 vendored here, 26 addable) |
| Stacked chrome above the console | 146px = 17% of an 857px viewport |
| Fill ratio of the HDMI/Serial tab band | ~16% content, ~84% empty |
| Peer KVM products using tabs for video-vs-serial | 0 of 5 surveyed |
| Unused component JS shipped to every browser | ~33.6 KB (bundle ~197 KB pre-gzip) |

**The codebase is in better shape than the screen is.** Component discipline is high: there are no
hand-rolled dropdowns, buttons, or tabs where the library provides them, every deviation carries a
why-comment at the site, and the `hidden`-attribute state idiom is documented and consistently
applied. The visible problems are compositional, not architectural.

**Three findings drive the redesign:**

1. **Three stacked chrome bars** (navbar 44px, tab band 52px, panel toolbar 50px) consume 17% of the
   viewport and are set in the same 12px ALL-CAPS voice, so they read as one undifferentiated zone.
2. **The tab band is the weakest bar** — two triggers, ~84% empty width, and its selected label is
   restated verbatim by the panel toolbar 53px below.
3. **The navbar mixes five interaction semantics** (status readout, dropdowns, panel toggles,
   external link, session actions) into one identical ghost-button treatment.

**The fix:** replace the tab band with a segmented control inside a two-tier header — tier 1 states
facts (status pills), tier 2 offers actions. This is the two-bar design `ui/components/navbar.templ:8-12`
already declares in a comment; the tab strip is the unplanned third bar that arrived with the HDMI
feature. Feasibility is verified in source (§8) — the mode switch can relocate with essentially no
JavaScript changes.

---

## 2. Component selection guide

### 2.1 Decision table — UI need to component

This is the operative "which component when" table. Prefer it over reasoning from component names.

| UI need | Use | Status | Rationale |
| --- | --- | --- | --- |
| Any action trigger | `button` | vendored | App-wide workhorse; `Href` renders an `<a>` |
| Group related **actions** | `buttongroup` | add | Library rule: ButtonGroup for actions, ToggleGroup for state |
| Mutually exclusive **view mode**, 2–5 options, instant apply, no URL change | `togglegroup` (single, outline) | add | This is the HDMI/Serial switch — see §8 |
| On/off control needing visible pressed state | `toggle` | add | Fixes the Overview-vs-Keyboard active-state inconsistency (§5.3) |
| Peer content sections in the same context | `tabs` | vendored | Correct for Virtual Media's Existing/Upload/URL. **Wrong for console modes** (§7) |
| Action menu from a button | `dropdownmenu` | vendored | Power, Macros. Not for multi-step flows |
| Multi-step flow that must survive click-away | `dialog` (or `sheet`) | vendored | Virtual Media belongs here, not in a dropdown (§5.4) |
| Blocking destructive confirmation | `alertdialog` | vendored | Backdrop-dismiss is impossible by design; already wired to `hx-confirm` |
| Status/state chip | `badge` | vendored | Wrap in a shared `statusPill`; see the padding caveat in §5.4 |
| Key/value readout row | `item` via `statusRow` | vendored | Presentation only — form controls use `field` |
| Form label + control + help + error | `field` | vendored | The altitude rule, already stated at `settings_menu.templ:711-714` |
| Hint for an icon-only button | `tooltip` | add | Expand vs Fullscreen glyphs are indistinguishable at 14px (`console_toolbar.templ:90-107`) |
| Structured empty state | `empty` | add | Serial idle hint, no-macros state; replaces ad-hoc overlay markup |
| Loading placeholder | `skeleton` | add | Overview cards and HTMX fragments; zero JS |
| Tabular data (sensors, sessions, logs) | `table` | add | Zero JS, scrollable wrapper |
| Section collapse | `accordion` / `collapsible` | add | Progressive disclosure, max two levels |
| Divider | `separator` | vendored | Replaces hand-rolled `w-px`/`h-px` divs (§3.3) |
| Async/form feedback | `toast` | vendored | Mounted app-wide; SSR adoption is HTMX-safe |
| Command palette | `command` | add | Natural later fit over power/media/macro actions |
| Sensor/telemetry graphs | `chart` | add | Explicitly survives HTMX swaps with no wiring |
| Choice of >6 or rare options | `select` / `dropdownmenu` | vendored | Mind the Select portal caveat below |

**Two caveats that have already bitten us:**

- `select` portals its content to `<body>`, and `dropdownmenu` treats pointerdowns there as
  outside-clicks — so a composed Select inside a dropdown dismisses the dropdown. `NativeSelect`
  exists for exactly this case (`power_menu.templ:82-86,154-161`).
- `badge` base classes are `rounded-none border-0 bg-transparent px-0 py-0` (`badge/badge.templ:59`).
  Layering a tint class on without restoring padding produces a highlighter smear, not a pill.

### 2.2 Full catalog (52 components)

All 52 slugs verified live at `https://shadcn-templ.com/docs/components/<slug>`. Five plausible
slugs do **not** exist — do not plan around `scroll-area`, `sonner`, `menubar`, `navigation-menu`,
or `data-table`.

Legend: **V** = vendored in `ui/components/` · **A** = available via `shadcn-templ add <slug>`

| Component | | Purpose | Use when |
| --- | --- | --- | --- |
| accordion | A | Stacked headings each revealing content; multi-open | Collapsible settings sections |
| alert | V | Inline callout, Default/Destructive | A condition needing awareness, in-flow |
| alert-dialog | V | Blocking modal, no backdrop dismiss | Critical/destructive confirmation |
| aspect-ratio | V | Constrain to a ratio via `--ratio` | Media that must hold 16/9 (unused today) |
| avatar | A | Image with fallback, groups, status badge | User identity; low relevance for a single-admin BMC |
| badge | V | Compact chip, 6 variants | Status/state chips |
| breadcrumb | A | Path navigation | Only if real page hierarchy appears |
| button | V | 6 variants × 8 sizes; Href renders anchor | Every action trigger |
| button-group | A | Groups buttons, strips inner radii, split buttons | Associating multiple actions |
| calendar | A | Month grid, ranges, constraints | Date input; unlikely here |
| card | V | Header/Title/Description/Content/Footer | Grouping related content |
| carousel | A | Swipeable slider | Not applicable |
| chart | A | Area/Bar/Line/Radar/Radial/Pie, own runtime | Telemetry graphs; HTMX-swap safe |
| checkbox | V | Binary control + hidden native input | Binary form choices |
| collapsible | A | One expand/collapse region | A single Advanced block |
| combobox | V | Autocomplete + list, chips multi-select | Searchable choice (unused; 22.7 KB JS still ships) |
| command | A | cmdk port, fuzzy palette | Ctrl+K action palette |
| context-menu | A | Right-click menu, nesting, checkbox/radio | Right-click on the console surface |
| date-picker | A | Popover + Calendar composition | Unlikely here |
| dialog | V | Modal/non-modal, focus trap, Esc, portal | Forms and multi-step flows |
| drawer | A | Edge panel, swipe-dismiss, snap points; modal | Mobile panels (BMC terminal needs non-modal — stays hand-rolled) |
| dropdown-menu | V | Floating action menu, submenus, shortcuts | Action menus |
| empty | A | Media + title + description + content | Designed empty states |
| field | V | Label+control+help+error, orientations | Every form control row |
| hover-card | A | Rich hover preview | Low need; tooltip covers our cases |
| icon | V | Lucide 0.576.0, 1,702 icons as Go values | All iconography — never hand-roll an inline `<svg>` |
| input | V | Single-line input, file accept | All text entry |
| input-group | V | Addons/buttons attached to inputs | URL + Fetch field, copy buttons (unused) |
| input-otp | A | Segmented code entry | If 2FA ever lands |
| item | V | Media+title+description+actions row | Structured list rows (presentation) |
| kbd | V | Keycap display | Shortcut hints; virtual-keyboard keycaps |
| label | V | Accessible form label | Login/password forms |
| pagination | A | Page navigation, no JS | Event-log paging |
| popover | V | Floating rich content near a trigger | Anchored mini-panels (unused; 10.4 KB JS still ships) |
| progress | V | Progress bar; auto-registers HTMX-swapped bars | Upload/update progress |
| radio-group | A | Exclusive options, roving tab stop | Settings choices better shown open |
| resizable | A | Draggable panel groups, keyboard accessible | Console/terminal split view |
| select | V | Floating option list, type-ahead | Compact single choice (portal caveat above) |
| separator | V | Semantic divider, H/V, no JS | All dividers |
| sheet | A | Dialog-based side panel from any edge | Alternate home for Virtual Media |
| sidebar | A | Full app-nav system, collapse modes | Rejected for overview (covers navbar); only if IA moves to routes |
| skeleton | A | Pulse placeholder, no JS | Loading states |
| slider | A | Range input, multi-thumb | Video quality/bitrate later |
| spinner | V | Loading indicator, `role=status` | In-flight feedback |
| switch | V | On/off toggle + hidden native input | Settings toggles |
| table | A | Semantic table in scroll wrapper | Sensors, sessions, logs |
| tabs | V | Layered panels; `window.tui.tabs.setActive` | Peer content sections only |
| textarea | V | Multi-line input | Paste-text flows |
| toast | V | Timed feedback; SSR adoption via `data-tui-toast-ssr` | Async/form feedback |
| toggle | A | Two-state button, `data-pressed`/`aria-pressed` | Panel toggles needing pressed state |
| toggle-group | A | Single/multi toggle set = segmented control | The console mode switch |
| tooltip | A | Floating hint; shows on `:focus-visible` | Icon-only buttons |

### 2.3 Library conventions

- **Theming is CSS-variables-only, in OKLCH.** Semantic tokens under `:root`/`.dark`; one `--radius`
  derives the whole radius scale. The docs' rule is *never hardcode colors* — which directly indicts
  our 11+ hardcoded status-color sites (§3.3).
- **One JS bundle, mounted once** via `@components.Scripts()` in the layout head; per-component
  vanilla IIFEs concatenated, Floating UI core before dom.
- **HTMX-friendliness is designed in.** Progress, Tabs, Input-OTP, and Command re-initialize via
  MutationObserver; Toast adopts SSR stubs; Chart survives swaps. Prefer these built-in observers
  over hand-written re-init glue.
- **Sanctioned integration points** are custom DOM events (`toggle-group-value-change`,
  `calendar-change`, `slider-change`, …) and `window.tui.*` APIs — not innerHTML surgery. Portaled
  content (popover, select, date-picker) needs delegated listeners.
- **Semantic pairings:** ButtonGroup = actions, ToggleGroup = state; Item = presentation rows,
  Field = form controls; AlertDialog when backdrop-dismiss must be impossible.
- **Form-post friendliness:** checkbox, radio, switch, select, combobox, and slider each keep a
  hidden native input. Give them `Name` and plain HTML form posts work — exactly right for HTMX.
- **After every `shadcn-templ add`:** run `templ generate && go mod tidy`.

---

## 3. Current component usage

### 3.1 Vendored and used

21 of the 29 vendored package directories are imported by app-level templ files (`floatingui` is
JS-only but load-bearing at runtime). Workhorses: `button`, `icon`, `item`, `badge`, `dropdownmenu`.

| Component | Used by |
| --- | --- |
| button | navbar, bmc_terminal, console_toolbar, video_panel, overview, settings_menu, power_menu, virtual_keyboard, virtual_media, macro_bar, confirm_dialog, pages/{api_docs,login,password} |
| icon | essentially every app component and page |
| item | overview, settings_menu, status_row, virtual_media, pages/api_docs |
| badge | console_toolbar, overview, video_panel, pages/api_docs |
| dropdownmenu | power_menu, virtual_keyboard, macro_bar, virtual_media |
| input | settings_menu, console_toolbar, virtual_media, pages/{login,password} |
| card | overview, settings_menu |
| dialog | settings_menu, pages/login |
| select | settings_menu, overview |
| tabs | pages/home (console tabs), virtual_media (Existing/Upload/URL) |
| field, switch, textarea | settings_menu |
| checkbox | console_toolbar (search options) |
| kbd | virtual_keyboard (every keycap) |
| label, alert, spinner, progress | pages/{login,password}, virtual_media |
| alertdialog | confirm_dialog |
| toast | layouts/base (Toaster mounted app-wide) |

**No hand-rolled dropdowns, buttons, or tabs exist where the library provides them.** Every bypass is
either a documented library limitation or a genuinely app-specific surface (keyboard matrix, video
overlay).

### 3.2 Vendored but unused

| Component | Cost |
| --- | --- |
| `combobox`, `popover`, `inputgroup` | ~33.6 KB of JS shipped to every browser — `scripts.go:29-46` concatenates all `components/*/*.js` regardless of use |
| `aspectratio`, `separator` | No JS cost, but `separator` is unused *while dividers are hand-rolled* |
| `componentexample/`, `example/` | Zero importers, no route; `componentexample_templ.go` alone is 145 KB of generated Go compiled into the BMC binary |

### 3.3 Smells and tech debt

| Smell | Detail | Where |
| --- | --- | --- |
| Toolbar duplication | `videoToolbar` mirrors `ConsoleToolbar` in container classes, badge cluster, and button set — acknowledged in a comment, never extracted (~70 duplicated lines) | `video_panel.templ:50-127` vs `console_toolbar.templ:20-109` |
| Hardcoded status colors | `bg-green-500/15 text-green-500` and friends re-spelled at 11+ sites across six components; no `--success`/`--warning` token exists | console_toolbar, video_panel, overview, power_menu, virtual_media, `virtual_keyboard.templ:43` (the app's only blue) |
| Two visibility idioms | Convention is the `hidden` **attribute**; ConsoleSearchBar still toggles `hidden`/`flex` **classes** | `console_toolbar.templ:113-115`, `console_script.templ:180-193` |
| Duplicated boot-device list | Two hand-maintained copies of the same six targets, "kept in sync" by comment | `power_menu.templ:143-152`, `overview.templ:326-353` |
| Copy-paste twins | `settingsRow`/`settingsRowWide` differ only in Field orientation | `settings_menu.templ:617-667` |
| Misplaced shared primitive | `NativeSelect` used by two components but defined inside power_menu; its class string hand-copies Input styling | `power_menu.templ:157-161`, used at `virtual_media.templ:195` |
| Global-scope offender | `console_script` declares top-level globals; every other script IIFE-wraps specifically to dodge it | `console_script.templ:8-18` |
| Hand-rolled dividers | `<div class="w-px h-3.5 bg-border">` while `separator` sits unused | `navbar.templ:22,54`, `power_menu.templ:76` |
| Inline SVGs | 4 hand-written chevrons despite 1,702 generated icons; two parallel method-pill implementations | `api_docs.templ:131,381,491,553`; `:159-163` vs `:294-300` |
| Only inline style in the app | `style="height:2.75rem"` — `h-11` is exactly that | `navbar.templ:14` |
| Dead dispatch | "Edit macros…" fires `macro-editor:open`; no listener exists anywhere | `macro_bar.templ:122` |
| App JS in the vendor bundle | `virtual_media.js` is served inside `/components/shadcn-templ-<hash>.js` | `scripts.go:15-16` |

---

## 4. Conventions to preserve

These are standards the codebase already follows. They are recorded here so they survive refactors
and new contributors.

1. **No library bypass without a why-comment at the site.** Overview rejects `sidebar` (it would
   cover the navbar, `overview.templ:23-25`); BMC terminal rejects `sheet`/`drawer` (must stay
   non-modal and drag-resizable, `bmc_terminal.templ:21-24`); `NativeSelect` exists because of the
   Select-portal collision (`power_menu.templ:82-86`). **This is the rule to keep verbatim.**
2. **The hidden-attribute idiom.** Server-render every state variant; JS flips the `hidden` DOM
   property — never innerHTML, never a `hidden` class (which loses to component display utilities
   and is stripped by TwMerge). Documented at `console_toolbar.templ:24-29`, `console_script.templ:25-30`,
   `video_panel.templ:55-56`, `confirm_dialog.templ:66-68`.
3. **Shared row primitives.** `statusRow`/`statusValue` (`status_row.templ:9-36`) keep every
   key/value readout in one rhythm by construction. The settings row family wraps Field/Item with
   the altitude rule stated in source (`settings_menu.templ:711-714`).
4. **HTMX conventions.** Fragment routes under `/ui` render the *same* templ functions as first
   paint (`ui/fragments.go:9-12`); event-driven refresh via `HX-Trigger` (`overview.templ:27-29`);
   lazy panel loads bound to trigger clicks (`settings_menu.templ:191-200`); `hx-disabled-elt` on
   mutating buttons; one central bridge (`htmx.templ:10-63`) wires `hx-confirm` → AlertDialog and
   server toasts so fragments never ship their own glue; auth-aware fragments answer `HX-Redirect`.
5. **Push state, not polling.** Live power state arrives over SSE (`EventSource('/api/vm/gpio/events')`,
   `power_menu.templ:175`) with last-known state deliberately left on screen while the source
   retries (`:182`); video signaling rides WebRTC.
6. **Destructive-action hygiene.** Declarative `hx-confirm` plus consequence copy ("The host loses
   power immediately without a clean shutdown", `power_menu.templ:62-73`), rendered by one shared
   AlertDialog.
7. **Accessibility floor.** `aria-label` + `title` on icon-only buttons, maintained `aria-expanded`,
   `inert` on the offscreen keyboard (`virtual_keyboard.templ:30`), sr-only dialog headers,
   keyboard-operable clickable rows (`settings_menu.templ:142-145`).
8. **Resource-aware UX.** HDMI encodes only while watched — connect on tab-show, disconnect on
   tab-hide (`video_panel.templ:410-418`), because the SoC has one core. **Any redesign must preserve
   these lifecycle hooks.**
9. **Data handoff without string interpolation.** `templ.JSONScript` for the HID keymap sourced from
   `pkg/hid.KeyCodes` (`hid_input.templ:8-12,26-29`); data attributes for ICE servers with the
   escaping rationale written down (`video_panel.templ:17-21`).
10. **Naming and ID discipline.** Page-stable element IDs as Go consts (`consoleTabsID`,
    `overviewSidebarID`); view models in sibling `*_data.go` files with the zero-value-renders-
    placeholders convention; icons passed as first-class Go values.
11. **Container-query responsiveness.** Complex forms adapt with `@container/field-group` and
    `@md/field-group:` variants (`field/field.templ:168`, `settings_menu.templ:332,723`) rather than
    viewport breakpoints.
12. **Server-rendered state everywhere.** Fragments re-render from persisted config, so a rejected
    value never lingers on screen — an error-prevention property most SPAs get wrong.

---

## 5. Current UI assessment

Measured from nine headless captures of `10.42.0.19/dashboard` at 1600×857.

### 5.1 The console-tab problem, measured

| Band | Height | Contents |
| --- | --- | --- |
| Navbar | 44px | 11 controls, 5 interaction semantics |
| Tab band | 52px | 2 triggers, ~84% empty width |
| Panel toolbar | 50px | Identity, status badges, connect, view tools |
| **Total chrome** | **146px** | **17.0% of viewport; first terminal pixel at y=146** |

Five compounding causes make it read as awkward:

1. **A full-width band for two items.** Trigger content spans ~256px of 1600px. `tabs.List` is
   `w-fit self-start`, so the component is not full-width — but nothing else shares the strip, so
   the eye parses all 52px as a third bar.
2. **Triple label redundancy.** The tab row's only unique payload is *the other pane's name*; the
   toolbar already names the current pane 53px below.
3. **Duplicate chrome grammar.** Both bands are dark strips of `text-xs` uppercase items, and the
   2px underline indicator is easy to miss.
4. **No typographic hierarchy.** Buttons use `tracking-widest` (`button/button.templ:50`), tabs use
   `tracking-wider` (`tabs/tabs.templ:132`) — otherwise the same 12px ALL-CAPS semibold voice. The
   navbar already hosts sibling view-ish toggles, so HDMI/SERIAL CONSOLE read as navbar items that
   fell off the bar.
5. **It breaks the declared design.** `navbar.templ:8-9` documents a two-bar pattern; the tab strip
   is the unplanned third bar.

The tab band is also the only bar without working controls: the navbar carries 11 interactive
elements, the toolbar 5–6, the tab band 2 — an order of magnitude less density at the same cost.

### 5.2 Navbar assessment

Eleven items, **five interaction semantics, one visual treatment**: a live status readout
(● POWER OFF, which reads as an imperative destructive command but is actually a state pill,
`power_menu.templ:36-46`), two dropdowns, three panel/dock/drawer toggles, an external link, a modal
trigger, and logout — all identical ghost buttons.

- **Divider grouping maps to no nameable rule.** Virtual Media (host-facing) groups with power, BMC
  Terminal (BMC-facing) is isolated, and read-only Server Overview heads the tools group. A cleaner
  split is by object: host · BMC · meta.
- **The right cluster inverts emphasis.** Rarely used API DOCS carries the only text label while
  everyday Settings is icon-only.
- **Panel toggles lack a uniform pressed convention.** KEYBOARD shows an active background;
  SERVER OVERVIEW does not, despite being the same control class.
- **Crowding trajectory.** Nine left-cluster items at 1600px leave no room to grow, and labels
  collapse below `sm` (`hidden sm:inline`), turning the bar into 11 unlabeled glyphs on a tablet.

### 5.3 Cross-surface consistency

| Divergence | Evidence |
| --- | --- |
| Four dismiss idioms | Virtual media text CLOSE (`virtual_media.templ:88-95`), overview icon-only ✕ (`overview.templ:49-59`), settings "← Back to KVM" (`settings_menu.templ:113-120`), keyboard HIDE |
| Two title scales, same role | `card.Title` default 18px uppercase in settings vs overridden `text-sm` in overview (`card/card.templ:109`, `overview.templ:107`) — visible simultaneously |
| Two label grammars | Overview rows whisper (normal-case muted, `status_row.templ:17`); settings value rows shout (xs semibold uppercase, `settings_menu.templ:714`) |
| Two persistence models | General auto-saves on change, Network batches behind Save. Both *do* toast ("Settings saved", `ui/fragments_settings.go:104,120`) — the gap is that nothing marks which regime a control is in |
| Panel-surface zoo | Adjacent buttons open a dropdown, a push sidebar, a bottom dock, a slide-up drawer, and a modal. Virtual Media is the misfit — its CLOSE button and multi-step upload flows are a dialog wanting out |
| Universal ALL-CAPS | Buttons, badges, tabs, card titles, dialog titles, menu items, labels all uppercase with tracking — the single token choice that makes the three bars blur |
| Badge chrome | Zero-padding base classes make status tints hug text like highlighter smears beside padded buttons (`badge/badge.templ:59`) |
| False-positive error toast | The global `htmx:sendError` handler emits "Network error — The BMC did not respond" for *any* dropped request including background polls; the source comment even notes status 0 is expected during restarts (`htmx.templ:60-62`) |

### 5.4 What is already good

- **Dark-theme discipline** — every surface draws from one token set; no stray light surfaces across
  nine captures.
- **The HDMI empty state is textbook** — icon, diagnosis, remedy, and deliberately overlaid so a
  stale frame cannot impersonate a live one (`video_panel.templ:31-40`).
- **Informative console chrome** — `/dev/ttyS1 @ 115200 8N1` in muted text beside a three-state badge.
- **Toolbar parity between panes** prevents reflow when switching (`video_panel.templ:47-49`).
- **Monospace right-aligned values with em-dash placeholders** and provenance footers ("Read from
  host-reported inventory via Redfish").
- **Keyboard dock affordances** — DETACH/HIDE are verbs, layout picker labeled, lock-key indicators
  present, and the navbar button shows a real active state.
- **Push-not-overlay overview sidebar** shrinks and refits the console instead of covering it.
- **Login failure copy** distinguishes bad credentials, locked account, and rate limiting.

The notable gap: **the serial console has no empty state.** A connected-but-silent port is an
undifferentiated black void, asymmetric with its sibling pane.

---

## 6. JetKVM comparison

JetKVM's dashboard (React + Tailwind; analyzed from `github.com/jetkvm/kvm` source and docs — the
live device at `10.0.189.23` is password-locked) is a CSS-grid stack: **DashboardNavbar** (logo +
hostname left; status pills right) → **ActionBar** of XS icon+label buttons directly above the video
→ MacroBar → letterboxed video over a blueprint-grid background → optional InfoBar. Consoles
(kvm/serial/cdc-acm) share one xterm.js surface that slides up over the video; the mode is picked
from an ActionBar split-button caret. Settings is a separate route with left nav and uniform rows.

| # | Strength | What it looks like | Portability |
| --- | --- | --- | --- |
| 1 | Two-row header | Row 1 identity + passive status; row 2 actions scoped to the video. Status never mixes with actions | Direct — §8 is this |
| 2 | Designed empty/error states | Never a black rectangle: centered cards (icon, heading, troubleshooting bullets, "Try again") over a CSS radial-gradient grid (20px cells, 0.5px lines) | Pure CSS + templ; best quality-per-effort |
| 3 | Console as overlay, not tabs | One terminal surface over the video; Esc dismisses; focus diverts while open | Informs §8; our two co-equal panes argue for a segmented control instead |
| 4 | State-dot micro-indicators | Blue dot on Virtual Media while mounted; popover shows source/name/size/mode + Unmount, or a designed empty state | One-for-one on our trigger + dialog |
| 5 | Honest cascading status | USB shows disconnected whenever the transport is down; MacroBar's fieldset disables when not connected | Adopt as a rule for tier-1 pills and tier-2 disabled states |
| 6 | Settings IA | Uniform rows (title/description left, control right), badges, danger theme, inline spinners | Ours already rhymes; converge the row grammar (§5.3) |
| 7 | Stats on demand | RTT/jitter/delay/loss/FPS in a toggleable sidebar; header keeps only the pill; InfoBar mirrors target keyboard LEDs | Overview sidebar can host a stats card; LED mirror is a real gap |
| 8 | Progressive disclosure | Dev-mode gates the raw terminal; split-button carets hide rarer siblings; embed mode strips chrome | Maps to macros/HID advanced settings and a future kiosk view |

**Design-language note.** JetKVM commits to one accent, a slate ramp, weight-driven hierarchy in
normal case, and flat surfaces separated by one-step luminance — while our UI spends ALL-CAPS plus
tracking on nearly every element. That restraint is what makes their status pills and lone
mounted-media dot legible instantly.

---

## 7. Peer patterns and design guidance

**No surveyed product uses tabs for video-vs-serial switching.** Two families exist: dedicated KVM
devices keep one console screen and surface the terminal as an overlay or window from a toolbar
button; full BMC dashboards make KVM and SOL separate sidebar routes. Serial access is consistently
secondary and progressively disclosed. Pop-out to a dedicated window is table-stakes — and we lack it.

| Product | Console-mode switching | Status placement |
| --- | --- | --- |
| JetKVM | ActionBar split-button caret; terminal slides up over video | Header pills + bottom InfoBar |
| PiKVM | System → "• Term" opens a draggable window beside the stream; host serial not integrated (open request #1526) | Header LED row (color-only — the accessibility anti-pattern) |
| TinyPilot | No serial console; View menu toggles client-local UI | Bottom status bar, dot + "Connected" text |
| OpenBMC webui-vue | KVM and SOL are separate sidebar routes; no in-page switch | Per page: status top-left as a definition list, actions top-right (a design-workgroup decision) |
| Proxmox VE | Header "Console" dropdown picks the viewer (noVNC/SPICE/xterm.js) | In-page |

### When to use which control

| Control | Right when | Wrong when |
| --- | --- | --- |
| Tabs | Alternating peer content views in one context; 2–6 items, one row, 1–2-word labels, ≥2 selection cues | Users must compare across tabs; the switch is a mode change; mixing URL-changing with panel-toggling triggers |
| Segmented control | Pick-and-instantly-apply among 2–5 presentations of the same content; no URL change | More than 5 options; selection navigates |
| Dropdown / split button | More than ~6 options, or rare choices; split button when one option dominates | Two frequent co-equal options — it hides state and adds a click |
| Sidebar routes | Global navigation between distinct functional areas | Mode switches inside one working surface |
| Progressive disclosure | Advanced/rare features one level down, split by frequency; max two levels | Burying the primary mode switch |

### Accessibility requirements for the target design

- **Action bar:** `role="toolbar"` + `aria-label` (3+ controls), a *single tab stop* with roving
  tabindex and arrow-key movement. Toolbar, not menubar — menubar is an OS-menu interaction contract
  we do not otherwise honor.
- **Segmented control:** group text label (screen-reader-only is fine); every icon-only segment gets
  `aria-label`; selection never signaled by color alone.
- **Status pills:** never color-only — dot **plus text**. Cap the cluster at ~5 and order
  most-severe first.
- **Overlays:** move focus in on open, Esc closes, focus returns to the invoker; `aria-modal` only
  if the background is truly inert.
- **Icon-only buttons:** `aria-label` plus tooltip; tooltips must not replace labels in critical
  workflows. Keep icon+label as the toolbar default — JetKVM and TinyPilot both do.

---

## 8. Redesign blueprint

### 8.1 Eliminate the HDMI/Serial tab row

**Option 1 — segmented control in the action bar. ✅ Recommended.**
A `togglegroup` (single-select, outline) with `icon.Monitor`+HDMI and `icon.Terminal`+Serial as the
leftmost element of tier 2. It matches segmented-control criteria exactly: instant apply, two
options, same underlying target, no URL change. Both modes stay visible at zero interaction cost —
which matters because the persisted *Primary View* setting can make either one the default, so a
caret menu would hide whichever is not current.

**Option 2 — mode dropdown in the panel toolbar. Rejected.**
The Proxmox precedent, but dropdowns suit >6 or rare choices. With two frequent options it adds a
click, hides the alternative, and creates a chicken-and-egg: the control that switches panes would
live *inside* a pane.

**Option 3 — entries in a View menu. Rejected.**
The TinyPilot precedent — but TinyPilot has no serial console, menubar ARIA is a heavyweight contract
we do not otherwise use, and burying the primary mode switch two interactions deep violates
frequency-based disclosure.

#### Feasibility, verified in source

- `tabs.js` resolves triggers and panes **document-wide** by `data-tui-tabs-id` and uses
  document-level click delegation (`tabs/tabs.js:5-43`), plus a public `window.tui.tabs.setActive`.
- The templ layer server-renders initial active state from `DefaultValue` through context
  (`tabs/tabs.templ:82,142-171`).
- Therefore the mode switch can live anywhere **inside the `tabs.Tabs` root wrapper** — and that
  wrapper can be widened to enclose the action bar plus both panes. `setupInitialStates` only scans
  within the container, so triggers rendered elsewhere are a safe no-op because the server already
  rendered the state.
- The existing lifecycle script also delegates at document level (`home.templ:145-148`), so lazy
  WebRTC connect/disconnect and xterm refit keep working untouched. If we move to `togglegroup`
  markup, switch that listener to the sanctioned `toggle-group-value-change` event.

**Payoff:** the 52px band disappears, triple label redundancy collapses to one, and the two-bar
design declared at `navbar.templ:8-12` becomes true again.

### 8.2 Two-tier header

**Tier 1 states what is true. Tier 2 offers what you can do.** Chrome drops from three bars/146px to
two bars/~90px.

```text
┌────────────────────────────────────────────────────────────────────────────────┐
│ ◆ NanoKVM · licheervnano     ● Host: On  ● 1920×1080@30  ● Serial: Up  ● Link  │   tier 1 — what is true
├────────────────────────────────────────────────────────────────────────────────┤
│ ┃▢ HDMI┃⌗ Serial┃ │ Power ▾  Media •  Keyboard  Macros ▾ │ Overview  BMC Term  │   tier 2 — what you can do
│                                       connect · search · mouse mode · ⛶  ←     │
├────────────────────────────────────────────────────────────────────────────────┤
│                                                                                │
│                       console surface (video / terminal)                       │
│                                                                                │
└────────────────────────────────────────────────────────────────────────────────┘
```

| Slot | Content | Moves from | Built with |
| --- | --- | --- | --- |
| Tier 1 · left | Logo + hostname | `navbar.templ:17-19` | plain markup + typography const |
| Tier 1 · status | Host power ("Host: On" — no longer imperative-reading "POWER OFF"), video signal/resolution, serial state, link health | badge clusters in `video_panel.templ:57-83` and `console_toolbar.templ:36-51`; power pill `power_menu.templ:36-46` | new shared `statusPill` primitive (own file, like `status_row.templ`) on `badge`, padded, dot + text |
| Tier 1 · right | Settings, logout; API Docs demoted into settings or an overflow menu | `navbar.templ:74-112` | unchanged button/dialog triggers, `dropdownmenu` for overflow |
| Tier 2 · mode | HDMI/Serial segmented control, server default from persisted Primary View | `home.templ:53-83` | `togglegroup` (add) |
| Tier 2 · host | Power menu (actions only — state moved to tier 1), Virtual Media (dialog + mounted dot), Keyboard, Macros | `power_menu`, `virtual_media`, `navbar.templ:39-53` | `dropdownmenu`, `dialog`, `toggle` (add) |
| Tier 2 · BMC | Overview toggle (finally gets the pressed state Keyboard already has), BMC Terminal toggle | `navbar.templ:23-70` | `toggle`; the drawer itself stays hand-rolled |
| Tier 2 · pane tools (right) | Connect/disconnect, search, mouse mode, expand/fullscreen — swapped per mode via the `hidden` attribute so switching never reflows | `console_toolbar.templ:20-109` + `video_panel.templ:50-127` | one shared toolbar primitive (kills the ~70-line duplication); `tooltip` on icon-only members |

**Semantics to carry over from JetKVM:** when the link pill is down, video and serial pills render
degraded and tier-2 controls that cannot work are `disabled` — never show green for a subsystem whose
parent transport is dead.

**Typography does the tier separation.** Keep the ALL-CAPS voice for tier-2 actions; drop tier-1
pills to normal case. Universal ALL-CAPS is precisely why the bars currently blur (§5.3). The pane's
identity string ("Serial Console · /dev/ttyS1 @ 115200") then lives once — in the selected segment
plus the tier-1 serial pill.

---

## 9. Roadmap and cleanup backlog

### Sequenced plan

1. **Foundations (de-risks everything else).** Add `--success`/`--warning` OKLCH tokens to
   `ui/assets/css/globals.css`; extract a `statusPill` primitive and a shared pane-toolbar
   primitive; fix badge padding at the primitive rather than per call site.
2. **Vendor adds.** `shadcn-templ add toggle-group toggle tooltip empty skeleton`, then
   `templ generate && go mod tidy`. Consider `sheet`, `table`, `command` later.
3. **Ship §8.1.** Replace the tab band with the segmented control — independently shippable,
   recovers 52px immediately, keeps `consoleDefaultTab` and the lazy-WebRTC lifecycle intact.
4. **Ship §8.2.** Split `navbar.templ` into `top_bar.templ` + `action_bar.templ`; move status up and
   actions down; wire honest cascading (link down ⇒ dependent controls disabled).
5. **Virtual Media → dialog** with a mounted-state dot on its trigger. Its internal
   Existing/Upload/URL tabs stay — that is a correct use of tabs.
6. **Polish.** CSS grid background behind the letterboxed video; a serial idle empty-state card
   (`empty`); scope or drop the global send-error toast (gate it on the link pill's state); align
   dismiss idioms and card-title scales.

### Cleanup backlog

- Delete `componentexample/` and `example/`; prune or stop bundling unused `combobox`/`popover`/
  `inputgroup` JS; move app JS out of the vendor bundle path.
- Implement or remove the `macro-editor:open` dispatch (`macro_bar.templ:122`).
- Adopt `separator` for hand-rolled dividers; `h-11` for the inline style; `icon.ChevronRight` for
  the api_docs inline SVGs; dedupe the method-pill twins.
- Share the boot-device list as one Go slice; merge `settingsRow`/`settingsRowWide`; move
  `NativeSelect` to its own file; IIFE-wrap `console_script`; migrate ConsoleSearchBar to the
  hidden-attribute idiom.
- Resolve naming drift: settings says "Back to KVM", the docs button says "Dashboard", the route is
  `/dashboard`. Pick one.

### Open questions

- **Console pop-out to a dedicated window** is table-stakes across peers (OpenBMC, Proxmox,
  TinyPilot, JetKVM). Worth a slot in tier-2 pane tools?
- **Target keyboard LED mirror** (Caps/Num/Scroll) à la JetKVM's InfoBar — the HID stack already has
  the data. Where should it surface?
- JetKVM live-UI walkthrough is still pending a device password.

---

## Appendix: method and confidence

Five parallel research lanes (shadcn-templ catalog, codebase inventory, JetKVM analysis, UX/peer
literature, live-capture critique) ran as an 11-agent workflow. Each lane's load-bearing claims were
re-verified by an independent adversarial checker against the primary source — re-fetching doc pages,
re-reading source, re-measuring screenshots. **45 of 48 claims confirmed; three were refuted and are
corrected in this document:**

| Refuted claim | Correction |
| --- | --- |
| "The docs index banner claims 58 components" | No such banner exists; the catalog is exactly 52 |
| "22 of 29 vendored packages are app-imported" | 21 — `floatingui` is JS-only with no templ importer |
| "Settings auto-save is silent" | Auto-saved panels *do* raise "Settings saved" toasts (`ui/fragments_settings.go:104,120`); the real finding is only that two persistence regimes coexist unmarked |

**Primary sources.** [shadcn-templ docs](https://shadcn-templ.com/docs/components) (all 52 component
pages plus theming, CLI, installation); this repo at branch `edk2`;
[jetkvm/kvm](https://github.com/jetkvm/kvm) (grounded repo queries, README, jetkvm.com docs);
pikvm/kvmd; tiny-pilot/tinypilot; openbmc/webui-vue (plus GUI Design Workgroup issue #25); Proxmox VE
GUI docs; Nielsen Norman Group (tabs, progressive disclosure, icon labels); Primer (segmented
control, navigation); WAI-ARIA APG (tabs, toolbar, menubar, modal dialog); PatternFly and Carbon
(status indicators). Live captures: nine states of `10.42.0.19/dashboard` at 1600×857, headless
Chrome.

A rendered version of this research, with diagrams, is published at
<https://claude.ai/code/artifact/466ff83a-b35b-4e3d-8307-319f3b737268>.
