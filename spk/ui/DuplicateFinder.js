/* Duplicate Finder — native DSM desktop application (ExtJS 3.4).
   Loaded into the DSM desktop page via ui/config; renders with DSM's own
   widget framework so it inherits DSM styling and theming. Talks to the
   package daemon through api.cgi. */

Ext.namespace('SYNO.SDS.DuplicateFinder');

(function () {
	'use strict';

	/* DSM keys every package URL on the package id, and the id differs
	   between builds: the hand-built spk installs as DuplicateFinder, the
	   SynoCommunity build as duplicatefinder. DSM loads this file through a
	   script tag whose src is /webman/3rdparty/<id>/DuplicateFinder-<stamp>.js
	   (measured on the DS916+), and api.cgi sits beside the script, so the
	   base is read off that tag rather than hardcoded. document.currentScript
	   is the tag while it runs; the scan over document.scripts is for a
	   loader that leaves it unset. The dev daemon and the jsdom harness serve
	   the script from their root, which yields /api.cgi — dev.go routes any
	   path containing api.cgi. No tag at all is a broken deployment, and it
	   fails here, loudly, rather than at the first request. */
	var API_BASE = (function () {
		var tags = [], all = document.scripts || document.getElementsByTagName('script');
		var i, path;
		if (document.currentScript) { tags.push(document.currentScript); }
		for (i = 0; i < all.length; i++) { tags.push(all[i]); }
		for (i = 0; i < tags.length; i++) {
			/* .src is the resolved absolute URL; keep the path only */
			path = String(tags[i].src || '')
				.replace(/^[a-z][a-z0-9+.\-]*:\/\/[^\/]*/i, '')
				.replace(/[?#][\s\S]*$/, '');
			if (/\/DuplicateFinder[^\/]*\.js$/.test(path)) {
				return path.replace(/\/[^\/]*$/, '/api.cgi');
			}
		}
		throw new Error('Duplicate Finder: no DuplicateFinder*.js script tag, cannot locate api.cgi');
	})();
	var PREFS_KEY = 'duplicateFinder.native';

	/* Results are paged, exactly the way File Station pages a big folder: the
	   grid shows one page at a time under DSM's own SYNO.ux.PagingToolbar
	   (numbered pages, ±5-page jumps, first/last, the item count and a
	   refresh button), and the daemon serves the pages. Duplicates page in
	   whole GROUPS — the keep-one-per-group selection rules need every
	   displayed group complete, and the daemon never splits one across
	   pages; the flat tools page in rows, like File Station. Search, match
	   refinement and column sorting are all applied by the daemon, because
	   filtering or sorting one page locally would silently act on a
	   fraction of the results.

	   File Station uses 1000 rows per page; the flat tools match that. The
	   grouped duplicates grid renders a header plus two-to-many rows per
	   unit, so it pages in 100 groups to keep a page comparable in size. */
	var PAGE_GROUPS = 100;
	var PAGE_FILES = 1000;

	/* Ext's default Ajax timeout is 30 s. Moves and exports legitimately run
	   far longer (an ext4 or cross-volume move copies the data; File Station
	   takes as long as it takes), so those requests get an effectively
	   unlimited timeout — aborting client-side would only detach the UI from
	   an operation the NAS continues anyway. Page fetches get headroom for
	   big windows on slow models. */
	var LONG_OP_TIMEOUT = 12 * 3600000;
	var PAGE_TIMEOUT = 60000;

	/* ------------------------------------------------------------ utils */
	function esc(s) {
		return Ext.util.Format.htmlEncode(String(s == null ? '' : s));
	}
	function fmtBytes(b) {
		b = b || 0;
		if (!b) return '0 B';
		var u = ['B', 'KB', 'MB', 'GB', 'TB'];
		var i = Math.min(Math.floor(Math.log(b) / Math.log(1024)), u.length - 1);
		var v = b / Math.pow(1024, i);
		return (i === 0 ? Math.round(v) : (v >= 100 ? Math.round(v) : v.toFixed(v >= 10 ? 1 : 2))) + ' ' + u[i];
	}
	/* Volume capacities go through DSM's own CapacityRender, which uses the
	   localized unit strings (fmtBytes is English-only). It takes MEGABYTES,
	   not bytes. One decimal matches DSM's own "11.6 TB / 20.9 TB" reading,
	   verified against a real volume. */
	function fmtCapacity(b) {
		return UxCapacity ? UxCapacity((b || 0) / 1048576, 1) : fmtBytes(b);
	}
	function fmtNum(n) {
		n = String(n == null ? 0 : n);
		var out = '', c = 0, i;
		for (i = n.length - 1; i >= 0; i--) {
			out = n.charAt(i) + out;
			if (++c % 3 === 0 && i > 0) out = ',' + out;
		}
		return out;
	}
	/* Counts in the TOP-TOOLBAR SUMMARY only. That toolbar has no spare width
	   at the window's minWidth (measured on-device: 2 px clear with the longest
	   "Match: Blake3 + Name + Modified + Created" label), so an exact count
	   pushes the search box off the window as soon as the numbers grow — 40 px
	   over at 34k duplicates, 132 px at 1.2M plus the "capped" suffix; a small
	   NAS has too few files to expose it. Six figures and up
	   therefore render compact, which keeps every part of the string visible
	   rather than ellipsising the reclaimable total away. Grid cells and the
	   selection footer keep fmtNum — only this one line is width-critical. */
	function fmtCount(n) {
		n = Number(n) || 0;
		if (n < 10000) return fmtNum(n);
		if (n < 1000000) return (n / 1000).toFixed(n < 100000 ? 1 : 0) + 'K';
		return (n / 1000000).toFixed(n < 10000000 ? 2 : 1) + 'M';
	}
	// Dates render exactly as the backend sends them (YYYY-MM-DD HH:MM:SS) —
	// the same format File Station uses. Escaped anyway: every renderer here
	// lands in innerHTML.
	function fmtDate(s) {
		return s ? esc(s) : '—';
	}
	// Cleans an absolute path the way the daemon's filepath.Clean does —
	// collapsed slashes, no "." or trailing "/" — so a comparison against a
	// list the daemon normalized (the interruption marker) is exact.
	function cleanPath(p) {
		var parts = String(p || '').split('/'), out = [], i;
		for (i = 0; i < parts.length; i++) {
			if (parts[i] === '' || parts[i] === '.') continue;
			if (parts[i] === '..') { out.pop(); continue; }
			out.push(parts[i]);
		}
		return '/' + out.join('/');
	}
	/* Text destined for an ext:qtip attribute. Ext QuickTips reads the
	   attribute back — by then the browser has entity-decoded it — and
	   renders that value as HTML (QuickTip.showAt → body.update). One
	   htmlEncode therefore protects only the attribute, not the tooltip: a
	   ZIP member name of "<img onerror=…>" inside a file on the NAS would
	   come through the daemon's evidence text and run in the tooltip.
	   Encoding twice makes the decoded value HTML-safe text, so the tooltip
	   shows the literal characters. */
	function qtipText(s) {
		return esc(esc(s));
	}
	function pad6(n) {
		var s = String(n);
		while (s.length < 6) s = '0' + s;
		return s;
	}
	function synoToken() {
		try {
			if (typeof _S === 'function') return _S('SynoToken') || '';
		} catch (e) { /* ignore */ }
		try {
			return (Ext.Ajax.defaultHeaders || {})['X-SYNO-TOKEN'] || '';
		} catch (e) { /* ignore */ }
		return '';
	}

	// Synology's themed widget classes — present on every DSM 7 desktop.
	// (The development harness registers equivalent stubs.)
	var UxToolbar = SYNO.ux.Toolbar;
	var UxGrid = SYNO.ux.GridPanel;
	var UxMenu = SYNO.ux.Menu;
	var UxButton = SYNO.ux.Button;
	// Verified live in DSM before use: SYNO.ux.RadioGroup extends checkboxgroup,
	// renders one input per item, groups them (clicking one clears the other)
	// and getValue() returns the checked radio, whose inputValue is the choice.
	var UxRadioGroup = SYNO.ux.RadioGroup || Ext.form.RadioGroup;
	// Single yes/no options are checkboxes (the RadioGroup above covers the
	// mutually-exclusive pairs). DSM's wrapper under either of its names when
	// present, stock Ext — which DSM's stylesheet also themes — otherwise.
	var UxCheckbox = SYNO.ux.Checkbox || SYNO.ux.FormCheckbox || Ext.form.Checkbox;
	var UxNumber = SYNO.ux.NumberField;
	var UxCombo = SYNO.ux.ComboBox;
	var UxText = SYNO.ux.TextField;
	var UxSearch = SYNO.ux.SearchField;
	var UxTree = SYNO.ux.TreePanel || Ext.tree.TreePanel;
	/* File Station's advanced-search From/To fields are SYNO.ux.DateTimeField
	   (measured in DSM 7.4: its picker is SYNO.ux.DateTimePicker with
	   clearType:'clear', hideClearButton:false, isAllDay:true — the year/month
	   dropdowns and the Today/Clear buttons). SYNO.ux.DateField's picker is the
	   plainer one, so prefer DateTimeField to match File Station exactly. */
	var UxDate = SYNO.ux.DateTimeField || SYNO.ux.DateField || Ext.form.DateField;
	var UxDateMenu = SYNO.ux.DateTimeMenu || SYNO.ux.DateMenu ||
		(Ext.menu && Ext.menu.DateMenu);
	// File Station pairs related search controls on one row with a
	// syno_compositefield; each child takes flex instead of anchor
	var UxComposite = SYNO.ux.CompositeField || Ext.form.CompositeField;
	/* Every dialog here is a SYNO.SDS.ModalWindow constructed with
	   `owner: <this app window>`. That one config is what makes DSM treat it
	   the way File Station's Settings/Properties dialogs behave, and all of it
	   is DSM's own code — do not hand-roll any of it again:

	     afterShow      pushes the dialog onto owner.modalWin and masks the
	                    owner through the COMPONENT's mask(), which is what
	                    installs the mousedown -> blinkModalChild handler
	     blinkModalChild raises the topmost modal child and blinkShadow(3)s it,
	                    so clicking the app window flashes the dialog's edges
	                    instead of burying it
	     beforeShow     centres the dialog on the owner, not the viewport
	     afterHide /
	     onBeforeDestroy call hideFromOwner(), which de-registers and unmasks

	   Two traps to avoid: masking with the raw `appWin.getEl().mask()` skips
	   the component path entirely, so no blink handler is installed AND
	   blinkModalChild — finding owner.modalWin empty — falls through to
	   raising the APP WINDOW over the dialog. DSM's unmask is also
	   reference-counted via maskCnt, so a raw Element.unmask() alongside it
	   desynchronises the count and can strand the mask. Verified in DSM 7.4
	   against File Station's own Settings dialog. */
	var UxModal = SYNO.SDS.ModalWindow;
	var UxToast = SYNO.SDS.ToastBox;
	var UxCapacity = SYNO.SDS.Utils && SYNO.SDS.Utils.CapacityRender;
	/* DSM's own usage bar. cls MUST be 'sds-ux-progressbar': DSM styles the
	   bar's inner classes only under specific app scopes (.sds-ux-progressbar,
	   .resource-monitor-widget) — there is no generic rule, so any other cls
	   renders a completely invisible bar (measured in DSM 7.4). showValueText
	   off because the card prints "X of Y used" underneath instead. */
	var VOL_BAR = (SYNO.SDS.Utils && SYNO.SDS.Utils.PercentageBar) ?
		new SYNO.SDS.Utils.PercentageBar({
			fixed: false, showValueText: false,
			cls: 'sds-ux-progressbar', barHeight: 6, marginTop: 6
		}) : null;

	/* DSM 7.4 design tokens, measured in the browser from
	   scripts/syno-vue-components/style/syno-vue-components.css, which defines
	   them on :root — so they resolve inside this app's window too. Using the
	   token rather than its literal value means DSM stays the single source of
	   truth; each carries its measured value as a var() fallback so an older
	   DSM without the token still renders (an unresolvable var() would
	   otherwise drop the whole declaration).

	   NOTE the blue: DSM 7.4's primary is #057FEB, not DSM 6's #0086e5. */
	function tok(name, fallback) { return 'var(--v-' + name + ',' + fallback + ')'; }

	/* DSM draws its checkboxes from a sprite that ux-all.css references on
	   .syno-ux-checkbox-icon. We cannot simply put that class on the checker
	   element: Ext's CheckboxSelectionModel matches its click target with
	   `t.className == "x-grid3-row-checker"` — exact string equality — so a
	   second class stops checkbox clicks working at all. Instead, read the
	   sprite URL off a throwaway probe and reference the image ourselves,
	   leaving the checker's className stock. Reading it rather than hardcoding
	   /scripts/ext-3/ux/images/svg/checkbox.svg means a DSM that relocates the
	   asset is followed automatically. Returns null when the class is unknown
	   (or there is no layout engine, as under jsdom in the test harness), in
	   which case the stylesheet omits the image and Ext's stock checkbox
	   shows — degraded but fully functional. */
	function dsmCheckboxSprite() {
		try {
			var p = document.createElement('div');
			p.className = 'syno-ux-checkbox-icon';
			p.style.cssText = 'position:absolute;left:-9999px;top:0;';
			document.body.appendChild(p);
			var img = window.getComputedStyle(p).backgroundImage;
			p.parentNode.removeChild(p);
			return (img && img !== 'none' && img.indexOf('url(') === 0) ? img : null;
		} catch (e) { return null; }
	}
	var CB_SPRITE = dsmCheckboxSprite();
	var BORDER3 = tok('color-border-tier3', 'rgba(198,212,224,0.4)'); // FS's divider everywhere
	var FONT1   = tok('color-font-tier1', 'rgb(65,75,85)');           // primary text
	var FONT2   = tok('color-font-tier2', 'rgba(65,75,85,0.6)');      // secondary text
	var FONT3   = tok('color-font-tier3', 'rgba(65,75,85,0.4)');      // tertiary/hint text
	var ACTION  = tok('color-action-normal', '#057FEB');              // links, accents
	var ACTHOV  = tok('color-action-hover', '#006DCC');
	var ITEMBG2 = tok('item-bg-tier2', 'rgba(5,127,235,0.1)');        // selected-row tint
	var ITEMBG1 = tok('item-bg-tier1', 'rgba(5,127,235,0.04)');       // hover tint
	var FSMALL  = tok('font-size-small-tier1', '12px');
	var FBASE   = tok('font-size-tier1', '13px');
	var R200    = tok('border-radius-200', '4px');

	/* Scoped stylesheet: only the rail, volume card, idle state and toast —
	   the parts of this UI that have no DSM counterpart. Everything DSM's own
	   theme paints (grid rows, headers, group headers, selection, hover,
	   dividers, the checkbox column) is deliberately NOT restyled here:
	   measurement shows ux-all.css already renders them identically to File
	   Station. Everything is scoped under .df-app. */
	Ext.util.CSS.createStyleSheet([
		// Rail rows carry DSM's own themed hover/selected classes
		// (x-grid3-row-over / -selected), so their highlight is painted by
		// DSM's theme and follows light/dark mode. Font-family is left unset so
		// it inherits DSM's UI font; size matches Control Panel's sidebar.
		// The constant transparent border matters: stock ext-all.css gives
		// .x-grid3-row-over/-selected a 1px border that grid rows already
		// reserve space for — without it here, hovering shifts the text.
		'.df-rail-item{display:flex;align-items:center;gap:11px;padding:9px 11px;cursor:pointer;font-size:14px;border-radius:' + R200 + ';margin:2px 8px;color:' + FONT1 + ';border:1px solid transparent !important;}',
		'.df-rail-ico{flex:none;width:20px;height:20px;}',
		'.df-rail-label{flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}',
		'.df-rail-item .df-badge{flex:none;margin-left:6px;color:' + FONT3 + ';font-size:' + FSMALL + ';font-variant-numeric:tabular-nums;}',
		/* toolbars: a little taller with contents vertically centred; the
		   right padding keeps trailing items (search box, Export) off the
		   window edge, like File Station */
		'.df-app .x-toolbar{padding-top:7px;padding-bottom:7px;padding-right:16px;}',
		'.df-app .x-toolbar td,.df-app .x-toolbar .x-toolbar-cell{vertical-align:middle;}',
		'.df-vol{padding:10px 12px;border-top:1px solid ' + BORDER3 + ';font-size:' + FSMALL + ';color:' + FONT2 + ';}',
		/* DSM's PercentageBar floats its track (it normally sits beside a
		   percentage label), so in this full-width card it would shrink to
		   zero and the fill would collapse to its 6px min-width. Un-float it
		   and let it span the card; the colours and radius stay DSM's. */
		'.df-vol .percentage-cmp-hbar-background{float:none;width:100%;margin-bottom:6px;}',
		/* fallback bar, used only if SYNO.SDS.Utils.PercentageBar is absent */
		'.df-vol .df-vol-bar{height:6px;background:' + tok('color-bg-grey', 'rgb(153,177,204)') + ';border-radius:3px;overflow:hidden;margin:6px 0;}',
		'.df-vol .df-vol-fill{height:100%;background:' + ACTION + ';}',
		'.df-idle{text-align:center;padding-top:90px;color:' + FONT2 + ';}',
		'.df-idle h2{font-size:' + tok('font-size-tier4', '16px') + ';color:' + FONT1 + ';margin:0 0 10px;font-weight:' + tok('font-weight-semi-bold', '600') + ';}',
		'.df-idle p{max-width:440px;margin:0 auto 18px;line-height:1.55;font-size:' + FBASE + ';}',
		'.df-loc{color:' + ACTION + ';cursor:pointer;}',
		'.df-loc:hover{color:' + ACTHOV + ';text-decoration:underline;}',
		/* The Hash column has no DSM counterpart, so there is no native value to
		   inherit — but it must not break DSM's 28px row rhythm. As a baseline
		   inline box, Menlo's metrics push the row taller than the cell's 28px
		   line-height (measured: 29px at 12px). inline-block + vertical-align:top
		   takes it out of baseline alignment and lands the row on exactly 28px,
		   matching File Station. */
		'.df-mono{font-family:Menlo,Consolas,monospace;font-size:' + FSMALL + ';display:inline-block;vertical-align:top;}',
		/* The top toolbar can run out of width: at the window's minWidth, with
		   every match criterion in the button's label, the summary and the
		   170px search box together overrun it. syncSummaryWidth measures that
		   for real and sets an inline max-width, clawing back only the pixels
		   the search box actually overflows by — a fixed cap here would be both
		   too tight on a wide window (clipping text with 400px of room to
		   spare) and too loose at minWidth with the longest label (the search
		   box still overrunning by 19px). This 380px stands only until that
		   first measurement runs. When a cap does bite, the count and group total —
		   the part users scan for — survive, and the full text stays available
		   as a tooltip. Ellipsis needs a block formatting context, hence
		   inline-block + middle alignment to keep the toolbar's rhythm. */
		'.df-summary{display:inline-block;max-width:380px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;vertical-align:middle;}',
		'.df-issue{white-space:pre-wrap;word-break:break-all;padding:1px 0;}',
		/* scope-dialog folder lists: rows are selectable (for Remove); the
		   transparent border absorbs ext-all's 1px hover/selected border so
		   the text never shifts on hover */
		'.df-app .x-list-body dl{cursor:pointer;padding:1px 3px;border:1px solid transparent !important;}',
		'.df-toast{position:fixed;left:50%;bottom:38px;transform:translateX(-50%);max-width:70%;',
		'background:#2b2f36;color:' + tok('color-white', '#FFFFFF') + ';padding:10px 16px;border-radius:' + R200 + ';font-size:' + FSMALL + ';',
		'line-height:1.45;z-index:200000;box-shadow:0 8px 30px rgba(0,0,0,0.35);}',
		'.df-group-waste{color:' + ACTION + ';}',
		'.df-group-more{color:' + FONT2 + ';}',
		/* These only recolour a border DSM draws — without them the stock
		   ext-all colour (a dark rgb(65,75,85)) shows through. */
		'.df-app .x-panel-body,.df-app .x-border-panel{border-color:' + BORDER3 + ';}',
		'.df-app .x-toolbar{border-color:' + BORDER3 + ';}',
		/* File Station's action toolbars carry no borders (its ttbar/btbar are
		   border:0). Match that — drop the full-width lines Ext draws under
		   the top toolbar and above the bottom toolbar (border:false makes
		   Ext add a tbar-bottom / bbar-top border). */
		'.df-app .x-panel-tbar .x-toolbar{border-bottom-width:0 !important;}',
		'.df-app .x-panel-bbar .x-toolbar{border-top-width:0 !important;}',
		/* …except the paging strip, which File Station DOES separate from the
		   rows above it. DSM's theme already styles that divider on
		   .syno-ux-pagingtoolbar (measured in File Station: 1px solid
		   rgba(198,212,224,0.4)); the rule above suppresses it, so only the
		   width is restored here — the colour stays the theme's, and so
		   follows light/dark mode. */
		'.df-app .x-panel-bbar .x-toolbar.syno-ux-pagingtoolbar{border-top-width:1px !important;}',

		/* Search menu's Reset/Search buttons, to match File Station's search
		   panel. Measured live: DSM's base .syno-ux-button is a 3px rectangle
		   and DSM re-rounds it to a 100px pill for buttons in a WINDOW footer —
		   which is why this app's dialog buttons (Move, Scope, Select Files)
		   are already correct and are deliberately not touched here. The search
		   form lives in an Ext MENU, which that rule does not reach, so its
		   footer buttons alone came out square. File Station hits the same gap
		   and patches it with an inline border-radius on each button; this does
		   it with one scoped rule instead. Toolbar buttons (Rescan, Scope…,
		   Move to…, Export) must STAY 3px — DSM's own toolbar buttons are
		   square, so widening this selector would break the match it exists to
		   preserve. The footer wrapper is .x-panel-btns, NOT .x-toolbar: the
		   buttons sit in a panel fbar, and a .x-toolbar selector silently
		   matches nothing (verified in DSM's live DOM). */
		'.df-search-menu .x-panel-btns .syno-ux-button{border-radius:100px;}',

		/* NO grid row / header / group-header rules here, on purpose.
		   Measured in DSM 7.4 by disabling this stylesheet and diffing
		   computed styles: on every text column DSM's own ux-all.css already
		   renders this grid byte-identically to File Station — 13px/400
		   headers in --v-color-font-tier2 with 0 8px padding,
		   rgba(198,212,224,0.4) dividers, rgba(5,127,235,0.04) hover,
		   rgba(5,127,235,0.1) selection, 28px rows, and group headers with
		   DSM's own collapse arrow. Rules here would either do nothing (DSM's
		   selectors are more specific) or diverge from File Station — an 11px
		   bold header, say, or a #f8f9fb header fill where File Station's is
		   transparent. Do not add any. */

		/* Checkbox column, painted with DSM's OWN checkbox art (see
		   dsmCheckboxSprite for why the image is referenced by URL rather than
		   by putting DSM's class on the element). Sprite rows are DSM's:
		   0=unchecked, -20=hover, -40=disabled, -60=checked, -120=grayed.
		   margin-top:4px is how DSM itself centres the 20px box in a 28px row
		   (see .syno-ux-grid-enable-column-*), which keeps the row at DSM's
		   28px (vertical-align:middle would inflate it to 29.5px).
		   background-position is pinned because ext-all.css sets 2px 2px on
		   .x-grid3-row .x-grid3-row-checker, which would offset the unchecked
		   box from the states below (all addressed at 0). */
		'.df-app .x-grid3-td-checker .x-grid3-cell-inner{padding-left:8px;padding-right:0;overflow:visible;}',
		'.df-app .x-grid3-row-checker{width:20px;height:20px;margin-top:4px;border:0;' +
			'background-repeat:no-repeat;background-color:transparent;background-position:0 0;' +
			(CB_SPRITE ? 'background-image:' + CB_SPRITE + ';' : '') + '}',
		'.df-app .x-grid3-row-selected .x-grid3-row-checker{background-position:0 -60px;}',
		/* Both unselectable, but for different reasons, so they read
		   differently. A read-only reference file is never a candidate at
		   all — it gets a padlock where the box would be (emitted by the
		   selection model's renderer, which is where the reasoning lives).
		   The last unselected file of a group IS an ordinary candidate that
		   happens to be blocked right now (tick a peer and it frees up), so
		   it keeps its box in DSM's disabled state. The -120px grayed row of
		   the sprite is deliberately unused: DSM spends it on a tri-state
		   parent whose children disagree, which is not what either of these
		   rows means. margin-top:4px is the same 20-in-28 centring DSM uses
		   for the checker itself, so the padlock sits on the row's baseline
		   rhythm rather than growing the row. */
		/* The padlock is the Unicode character U+1F512, not artwork, so it
		   needs no colour of its own (an emoji-presentation codepoint paints
		   from the font, not from `color`). A currentColor SVG here would
		   mis-inherit: ux-all.css carries
		   `.syno-ux-gridpanel div{color:rgb(65,75,85)}`, which matches any div
		   put in a grid cell directly. line-height pins the glyph to the 20px
		   box so a tall emoji font cannot grow the row. */
		'.df-app .df-prot-lock{width:20px;height:20px;margin-top:4px;' +
			'line-height:20px;font-size:13px;text-align:center;}',
		'.df-app .df-row-capped .x-grid3-row-checker{background-position:0 -40px;cursor:default;}',
		/* Same sprite, same meaning: a box that cannot be ticked. Styled off a
		   ROW class and never a second class on the checker itself — Ext's
		   CheckboxSelectionModel.onMouseDown compares
		   `t.className == "x-grid3-row-checker"` by STRING EQUALITY, so any
		   extra class there silently kills every checkbox click in the grid.
		   df-row-capped is styled this way for the same reason. */
		'.df-app .df-row-nomove .x-grid3-row-checker{background-position:0 -40px;cursor:default;}',
		/* Deliberately NOT greyed: unselectable rows keep normal text. Grey
		   text is this app's "Missing"/"Loading" signal, so spending it on
		   protection would make two unrelated states look alike. The disabled
		   checkbox and the padlock carry the signal on their own. */
		'.df-app .x-grid3-hd-checker{display:none;}', /* no select-all header box, per design */
		/* Conflicting Files verdicts. The row tint is deliberately faint — it has
		   to survive DSM's own alternating-row and hover backgrounds without
		   fighting them, and the Status column is what actually states the
		   verdict. Colours are DSM's semantic pair (the red is the same one
		   Storage Manager uses for a failing drive), not new inventions. */
		'.df-app .df-row-corrupt .x-grid3-cell-inner{background:rgba(226,118,108,0.10);}',
		'.df-app .df-verdict{display:inline-block;padding:0 8px;border-radius:9px;line-height:18px;font-size:11px;}',
		'.df-app .df-verdict-corrupt{background:rgba(226,118,108,0.18);color:#c0392b;}',
		'.df-app .df-verdict-intact{background:rgba(76,175,110,0.18);color:#2e7d4f;}',
		'.df-app .df-verdict-unknown{background:rgba(120,130,140,0.14);color:' + FONT3 + ';}',
		'.df-app .df-dim{color:' + FONT3 + ';font-style:italic;}',
		/* A grouped-grid header is drawn in a fixed 28px box, so a label needing
		   a second line does not grow the box — it overlaps the first row
		   underneath. Measured on the DS916+ with the longest corrupted-set
		   header (scrollHeight 56 against clientHeight 28).
		   This styles OUR OWN label span, never DSM's .x-grid-group-hd: the
		   standing rule is that DSM paints the grid, and smoke.js guards it.
		   Applies to the duplicates listing too, whose header can equally run
		   long on a narrow window. */
		'.df-app .df-group-label{display:block;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;}',
		/* scrollbars: DSM's apps replace the native bar with their fleXcroll
		   widget (8px translucent rounded thumb, no buttons). That widget has
		   no grouped-grid view, so we restyle the native bar to match. The
		   symmetric transparent border (asymmetric ones clip the pill shape
		   in WebKit) centres the 8px thumb in a 20px gutter, keeping it 6px
		   from the window edge so grabbing it never hits the resize handle. */
		'.df-app ::-webkit-scrollbar{width:20px;height:20px;background:transparent;}',
		/* No overflow-x rule here: DSM's own skin already computes
		   overflow-x:hidden on .x-grid3-scroller (measured), with and
		   without this stylesheet. The zero-height pseudo-element below still
		   earns its place — it removes the 20px gutter strip some WebKit
		   builds reserve, so the row area runs straight down to the bottom
		   bar's own line (one separator, like File Station). */
		/* DSM's grid skin pads every syno-ux-gridpanel 12px at the bottom —
		   room for exactly the paging strip this grid now carries, so the
		   padding is left in place (it separates the rows from the pager the
		   way File Station does). The right inset for the rows comes from the
		   view's scrollOffset, not from padding here — padding would drag the
		   fleXcroll anchor left along with the rows, putting the thumb left of
		   the row lines. */
		'.df-app .x-grid3-scroller::-webkit-scrollbar:horizontal{display:none;height:0;}',
		/* FleXcroll overlay thumb (the grid's scrollbar): stock flexcroll.css
		   uses -10px, which seats the thumb ~2px inside the row's right edge.
		   Measured in File Station, which uses exactly this — the thumb
		   right-justifies to (and overlaps) the table edge; -16px would float
		   it 6px too far in. Keep the stock value. */
		'.df-app .vscrollerbar{margin-left:-10px;}',
		'.df-app .hscrollerbar{margin-top:-10px;}',
		'.df-app ::-webkit-scrollbar-thumb{background-color:rgba(0,0,0,0.12);border-radius:100px;',
		'border:6px solid transparent;background-clip:content-box;}',
		'.df-app ::-webkit-scrollbar-thumb:hover,.df-app ::-webkit-scrollbar-thumb:active{background-color:rgba(0,0,0,0.42);}',
		'.df-app ::-webkit-scrollbar-track,.df-app ::-webkit-scrollbar-corner{background:transparent;}',
		'.df-app ::-webkit-scrollbar-button{display:none;}',
		/* trees (fallback folder picker + select-in-folder): File Station look */
		/* The trees are SYNO.ux.TreePanel, so DSM sizes the rows (28px) and
		   draws the expander from its own cate_tree_arrow.png (measured in
		   DSM 7.4). Hand-drawn row heights, corner radii, row margins or
		   chevrons would diverge from that; do not add any.
		   The hover/selected tints below are DSM's own token values, kept
		   because our nodes are plain (no iconCls) and to keep the selected
		   label legible. */
		'.df-app .x-tree-node .x-tree-node-el:hover{background:' + ITEMBG1 + ';}',
		'.df-app .x-tree-node .x-tree-selected{background:' + ITEMBG2 + ' !important;}',
		'.df-app .x-tree-node a{text-decoration:none;}',
		'.df-app .x-tree-node a span{color:' + FONT1 + ';}',
		'.df-app .x-tree-selected a span{color:' + ACTION + ';font-weight:bold;}',
		/* NAMED EXCEPTION to "inherit DSM": these nodes carry no iconCls, and
		   DSM then renders .x-tree-node-icon as a blank 16px gutter before
		   every label. Hidden until the scope/folder trees supply a real
		   folder iconCls, at which point this rule should go. */
		'.df-app .x-tree-node-icon{display:none;}',
		'.df-app input.x-tree-node-cb{margin:0 4px 0 0;vertical-align:middle;}'
	].join('\n'), 'duplicate-finder-css');

	/* -------------------------------------------------------------- api */
	/* opts.timeout overrides Ext's 30 s default per request. cb(err, json,
	   meta): meta.transport is true when no JSON answer arrived at all
	   (connection dropped, client-side timeout, proxy died mid-request) —
	   for a mutation that means the outcome is UNKNOWN, not failed: the
	   daemon keeps executing a move it has started even after the client
	   goes away. */
	function api(path, method, body, cb, opts) {
		var url = API_BASE + path;
		var tok = synoToken();
		if (tok) url += (url.indexOf('?') >= 0 ? '&' : '?') + 'SynoToken=' + encodeURIComponent(tok);
		Ext.Ajax.request({
			url: url,
			method: method,
			jsonData: body || undefined,
			timeout: (opts && opts.timeout) || undefined,
			headers: tok ? { 'X-SYNO-TOKEN': tok } : undefined,
			callback: function (o, success, resp) {
				var j = null, err = null;
				try {
					j = Ext.util.JSON.decode(resp.responseText);
				} catch (e) { /* not json */ }
				if (!success || !j || j.error) {
					err = (j && j.error) || ('Request failed (HTTP ' + resp.status + ')');
				}
				cb(err, j, { transport: !j });
			}
		});
	}

	/* Store proxy for the paged results API. DSM's own grids sit on stores
	   that page with offset/limit under a SYNO.ux.PagingToolbar, which is
	   exactly the shape of our /results endpoint — so the grid is driven by
	   a real paging store rather than by hand, and the toolbar's page
	   buttons, ±5 jumps, item count and refresh all work natively.
	   doRequest's contract is Ext's: hand records to callback(scope, result,
	   arg, success). The page JSON (whole duplicate groups, or plain rows)
	   is flattened to grid rows by the owning window, which also keeps the
	   whole-set totals the summary and badges report. */
	var DfPagingProxy = Ext.extend(Ext.data.DataProxy, {
		constructor: function (cfg) {
			this.win = cfg.win;
			// Ext refuses to issue a request unless the action has an api
			// entry; MemoryProxy marks 'read' the same way.
			var api = {};
			api[Ext.data.Api.actions.read] = true;
			DfPagingProxy.superclass.constructor.call(this, { api: api });
		},
		doRequest: function (action, rs, params, reader, callback, scope, arg) {
			var proxy = this, win = this.win, tool = win.state.tool;
			var body = Ext.apply({
				tool: tool,
				offset: params.offset || 0,
				limit: params.limit || win.pageSize(tool)
			}, win.fetchParams(tool));
			// Nothing aborts an outstanding request, and Ext does not sequence
			// them, so a slow earlier page (a search for "a" over a huge set)
			// could land AFTER the fast later one ("ab") and overwrite the
			// store, the group ids and the totals with the stale set. Every
			// request takes a number; a reply that is not the newest is
			// dropped.
			var seq = win._pageSeq = (win._pageSeq || 0) + 1;
			// A failed load must still end the load: Ext's LoadMask hides on
			// the store's load OR exception event, and Store.loadRecords with
			// success=false fires neither — so a failed page fetch would
			// leave "Loading…" over the grid and its pager for good. Firing
			// the proxy's exception (the store relays it) is what a stock
			// proxy does on failure, and what unmasks.
			var fail = function () {
				proxy.fireEvent('exception', proxy, 'response', action, arg, null, null);
				callback.call(scope, null, arg, false);
			};
			// A reply that is simply no longer wanted (an older request, or a
			// tool no longer on screen) is dropped WITHOUT the exception: the
			// newer load that made it stale ends the mask itself.
			var drop = function () { callback.call(scope, null, arg, false); };
			api('/results', 'POST', body, function (err, j) {
				if (seq !== win._pageSeq) {
					drop();
					return;
				}
				if (err || !j || !j.scanned) {
					if (err) win.notify(err);
					fail();
					return;
				}
				// The rail is not masked during a load (loadMask covers the
				// grid only), so a tool can be switched while a page is in
				// flight. Two responses then race into the one shared store,
				// last write wins, and the loser's rows render under the
				// winner's header, summary and pager. Drop a page whose tool
				// is no longer the one on screen — and when this reply is the
				// newest one (the tool switched to has no page load of its
				// own to end the mask, because it has no results), end the
				// mask here, or "Loading…" sits over the grid until the next
				// load.
				if (win.state.tool !== tool) {
					if (seq === win._pageSeq) fail(); else drop();
					return;
				}
				var page;
				try {
					page = win.pageToRows(tool, j);
				} catch (e) {
					fail();
					return;
				}
				callback.call(scope, reader.readRecords(page), arg, true);
			}, { timeout: PAGE_TIMEOUT });
		}
	});

	// Rail glyphs: small colour icons in the DSM Control Panel style (filled,
	// per-category colours). They stay colourful regardless of row selection,
	// like Control Panel's icons.
	function railIco(inner) {
		return '<svg class="df-rail-ico" width="20" height="20" viewBox="0 0 24 24">' + inner + '</svg>';
	}
	var TOOLS = [
		/* Two sheets of paper, not two squares: same body-plus-folded-corner
		   silhouette the Empty Files glyph uses, at the same ~36% fold, so the
		   two file categories read as the same object seen twice.
		   The back sheet stays an open stroke and shows only its cut corner —
		   its own fold triangle is exactly the part the front sheet covers, and
		   the stroke ends short of the front (2.7 above, 2.6 to the left) so
		   the break reads as occlusion rather than as a touching outline. */
		{ id: 'duplicates', label: 'Duplicate Files', ico: railIco(
			'<path d="M10.5 8.5 H16.2 L20 12.3 V19.5 A1 1 0 0 1 19 20.5 H10.5 A1 1 0 0 1 9.5 19.5 V9.5 A1 1 0 0 1 10.5 8.5 Z" fill="#3c8ae0"/>' +
			'<path d="M16.2 8.5 L20 12.3 H16.9 A0.7 0.7 0 0 1 16.2 11.6 Z" fill="#2f6cb0"/>' +
			'<path d="M13.2 5.8 L10 2.9 H4.6 A1.6 1.6 0 0 0 3 4.5 V13.2 A1.6 1.6 0 0 0 4.6 14.8 H6.9" fill="none" stroke="#3c8ae0" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"/>') },
		{ id: 'empty_folders', label: 'Empty Folders', ico: railIco(
			'<path d="M3 7.5 A2 2 0 0 1 5 5.5 H9 L11 7.5 H19 A2 2 0 0 1 21 9.5 V17 A2 2 0 0 1 19 19 H5 A2 2 0 0 1 3 17 Z" fill="#efa338"/>' +
			'<path d="M3 9 H21 V17 A2 2 0 0 1 19 19 H5 A2 2 0 0 1 3 17 Z" fill="#f6b959"/>') },
		{ id: 'empty_files', label: 'Empty Files', ico: railIco(
			'<path d="M6.5 3.5 H13.5 L19 9 V19.5 A1 1 0 0 1 18 20.5 H6.5 A1 1 0 0 1 5.5 19.5 V4.5 A1 1 0 0 1 6.5 3.5 Z" fill="#7ea8db"/>' +
			'<path d="M13.5 3.5 L19 9 H14.2 A0.7 0.7 0 0 1 13.5 8.3 Z" fill="#5a83b6"/>') },
		{ id: 'temp_files', label: 'Temporary Files', ico: railIco(
			'<circle cx="12" cy="12.5" r="8.3" fill="#efa63c"/>' +
			'<path d="M12 8 V12.7 L15.2 14.6" fill="none" stroke="#fff" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"/>') },
		/* "Conflicting Files", not "Corrupted Files": that name would assert
		   of every row what the scan can only prove of a few — a typical NAS
		   shows a hundred conflicting files and no convictions — so the
		   category name describes the DISAGREEMENT, which is what was actually
		   found, and leaves "Corrupted" to the per-row verdict that earns it.
		   A warning triangle, single-tone with a white glyph, the way the
		   Temporary Files clock is drawn — the other three icons' two-tone
		   bevel has nothing to bevel here. The corners are rounded by stroking
		   the path in its own fill colour with stroke-linejoin:round, which
		   avoids doing arc maths on three corners; ink spans x 2.6–21.4, the
		   same optical width as the icons above it. The red is #e2766c, the
		   token already used for the row tint and the Corrupted badge, not a
		   new colour. */
		{ id: 'corrupted_files', label: 'Conflicting Files', ico: railIco(
			'<path d="M12 4.6 L20.2 19 H3.8 Z" fill="#e2766c" stroke="#e2766c" stroke-width="2.4" stroke-linejoin="round"/>' +
			'<path d="M12 9.6 V14.2" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round"/>' +
			'<circle cx="12" cy="17.3" r="1.15" fill="#fff"/>') }
	];
	var TOOL_NAME = {};
	Ext.each(TOOLS, function (t) { TOOL_NAME[t.id] = t.label; });

	// Tools whose results are GROUPS rather than a flat list of rows. Paging
	// unit, the pager's count, the grouped grid, the row builder and the
	// summary all branch on this — none of them may test the literal
	// 'duplicates', which is correct only while it is the sole grouped tool.
	var GROUPED = { duplicates: true, corrupted_files: true };
	function isGrouped(tool) { return !!GROUPED[tool]; }

	// Tools that report but never act. Conflicting Files is one: which copy is
	// damaged is a judgement made from evidence, and the evidence is sometimes
	// silent — so the app shows what it found and leaves the decision, and the
	// deletion, to the user in File Station. The daemon enforces this too;
	// hiding the buttons here is presentation, not the guarantee.
	var READ_ONLY = { corrupted_files: true };
	function isReadOnly(tool) { return !!READ_ONLY[tool]; }

	// The scan that fills a tool in. Conflicting Files has no pass of its own —
	// it falls out of the duplicates scan, which is the only one that reads
	// file contents — so its Scan button runs that.
	function scanToolFor(tool) { return isReadOnly(tool) ? 'duplicates' : tool; }

	// The Status column's words. KEEP IN SYNC with verdictLabel() in
	// server/corrupt.go, which writes the same three verdicts into the CSV
	// export — if they drift ("Unknown" in the export against "Undetermined"
	// here) a report disagrees with the screen it came from. Both halves are
	// pinned by tests; the daemon's searchable synonyms (verdictSearchText)
	// accept either wording regardless.
	var VERDICT_TEXT = {
		corrupt: 'Corrupted',
		intact: 'Intact',
		unknown: 'Undetermined'
	};
	// The empty-folders tool lists directories, not files, and its batch
	// folder is literally called "Empty Folders" — so the move flow's own
	// wording follows the tool rather than saying "File" beside it.
	function itemNoun(tool, n) {
		var one = tool === 'empty_folders' ? 'Folder' : 'File';
		return n === 1 ? one : one + 's';
	}
	var IDLE_DESC = {
		duplicates: 'Find byte-for-byte identical files across your shared folders and reclaim wasted space. Matches are grouped so you can keep one copy and move the rest.',
		empty_folders: 'Locate folders that contain no files — or nothing but temporary junk like Thumbs.db — so you can tidy up the directory tree on your DiskStation.',
		empty_files: 'Find zero-byte files that serve no purpose and clutter your shared folders.',
		temp_files: 'Detect cache, backup and system junk such as Thumbs.db, desktop.ini, *.tmp and *.bak files.',
		corrupted_files: 'Find files that claim to be the same file — identical size and modified time — but hold different content. ' +
			'Nothing that edits a file leaves both of those untouched, so a difference underneath them points to bit rot, a bad sector ' +
			'or an interrupted copy. Where the evidence allows, each copy is marked Corrupted or Intact. Filled in by the Duplicate Files scan.'
	};

	/* Folder picking uses DSM's own FileChooser (the File Station "Move to…"
	   dialog). It works in share-space ("/Share/sub"), so results are mapped
	   back to real volume paths using the volume/share list. */
	var VOLUMES = [];   // filled by /info

	// Share name ↔ real path are connected ONLY by the {name, path} pairs
	// /api/info ships (from File Station's own list_share). They are not
	// derivable from each other: internal shares happen to use their name as
	// the directory (/volume1/Backups ↔ "Backups"), but external ones do not
	// — share "usbshare1" lives at /volumeUSB1/usbshare. Concatenating one
	// side from the other makes every USB destination answer "This location
	// cannot be used".
	function shareToReal(sharePath) {
		if (!sharePath) return null;
		var p = String(sharePath);
		if (p.indexOf('/') !== 0) p = '/' + p;
		var seg = p.split('/');
		var share = seg[1];
		if (!share) return null;
		var rest = seg.slice(2).join('/');
		var i, j, v, s;
		for (i = 0; i < VOLUMES.length; i++) {
			v = VOLUMES[i];
			for (j = 0; j < (v.shares || []).length; j++) {
				s = v.shares[j];
				if (s && s.name === share) {
					return s.path + (rest ? '/' + rest : '');
				}
			}
		}
		return null;
	}

	// Inverse of shareToReal: File Station navigates in share-space
	// ("/Backups/sub"), so real volume paths must be converted before
	// launching it — passed a "/volume1/…" path it silently falls back
	// to the share root.
	function realToShare(realPath) {
		var p = String(realPath || ''), i, j, v, s;
		for (i = 0; i < VOLUMES.length; i++) {
			v = VOLUMES[i];
			for (j = 0; j < (v.shares || []).length; j++) {
				s = v.shares[j];
				if (s && (p === s.path || p.indexOf(s.path + '/') === 0)) {
					return '/' + s.name + p.substring(s.path.length);
				}
			}
		}
		return p.replace(/^\/volume\d+/, '') || p;
	}

	// "Is this window still part of a flow in progress?" Destroyed and hidden
	// both count as finished, so a guard built on this cannot outlive the
	// window it is guarding — which matters for the chooser, whose cancel
	// path fires no callback of ours.
	function liveWin(w) {
		return !!w && !w.isDestroyed && (typeof w.isVisible !== 'function' || w.isVisible());
	}

	function pickFolder(cfg) {
		var chooser = new SYNO.SDS.Utils.FileChooser.Chooser({
			owner: cfg.owner || null,
			title: cfg.title,
			height: 500,
			usage: { type: 'chooseDir' },
			folderToolbar: true
		});
		chooser.mon(chooser, 'choose', function (c, sel) {
			var one = Ext.isArray(sel) ? sel[0] : sel;
			var real = shareToReal(one && (one.path || one.spath));
			if (!real) {
				if (cfg.onError) cfg.onError('This location cannot be used');
				return;
			}
			try { c.close(); } catch (e) { /* ignore */ }
			cfg.onPick(real);
		}, null);
		if (typeof chooser.open === 'function') chooser.open();
		else chooser.show();
		return chooser;
	}

	/* ===================================================== main window */
	SYNO.SDS.DuplicateFinder.MainWindow = Ext.extend(SYNO.SDS.AppWindow, {

		constructor: function (config) {
			this.state = {
				tool: 'duplicates',
				dirs: [],
				refDirs: [],
				match: { name: false, modified: false, created: false },
				// tool -> whole-set metadata for the summary and the rail
				// badges: {errors, scanMatch, total, grand, truncated}. The
				// rows themselves live in the grid's store, one page at a time.
				results: {},
				scanned: {},
				volumes: [],
				hashAlgo: 'Blake3',
				query: '',        // search filter, session-only
				// magnifier-menu options (File Station's advanced search),
				// session-only like query; all filtering is server-side
				searchOpts: { loc: '', ftype: '', extq: '', dateBy: 'modified',
					dateFrom: '', dateTo: '', sizeOp: '', sizeMB: null }
			};
			this.loadPrefs();
			this.pollTask = null;
			this.progressWin = null;
			this._flatSort = {};  // tool -> {field, dir} server-side sort

			this.buildComponents();

			config = Ext.apply({
				title: 'Duplicate Finder',
				cls: 'df-app',
				width: 1300,
				height: 760,
				// wide enough that the full toolbar — scan buttons, the Match
				// button with every criterion enabled ("Match: Blake3 + Name +
				// Modified + Created"), the summary and the search box — never
				// clips at the smallest size
				minWidth: 1300,
				minHeight: 540,
				resizable: true,
				maximizable: true,
				minimizable: true,
				// SYNO.SDS.AppWindow adds a "?" tool next to minimize unless
				// showHelp is already false when its constructor runs — it
				// opens SYNO.SDS.HelpBrowser on this app's jsID, and a
				// 3rd-party package has no page there, so the button only ever
				// leads to nothing.
				showHelp: false,
				layout: 'border',
				items: [this.westPanel, this.centerPanel],
				listeners: {
					scope: this,
					beforedestroy: this.onBeforeDestroy,
					// the summary's width budget is whatever the toolbar has
					// left, which changes with the window
					resize: this.syncSummaryWidth
				}
			}, config);
			SYNO.SDS.DuplicateFinder.MainWindow.superclass.constructor.call(this, config);

			this.on('show', this.bootstrap, this, { single: true });
		},

		/* --------------------------------------------------- preferences */
		loadPrefs: function () {
			try {
				var p = Ext.util.JSON.decode(window.localStorage.getItem(PREFS_KEY) || 'null');
				if (!p) return;
				if (Ext.isArray(p.dirs)) this.state.dirs = p.dirs;
				if (Ext.isArray(p.refDirs)) this.state.refDirs = p.refDirs;
				if (p.match) this.state.match = p.match;
			} catch (e) { /* ignore */ }
		},
		savePrefs: function () {
			try {
				window.localStorage.setItem(PREFS_KEY, Ext.util.JSON.encode({
					dirs: this.state.dirs,
					refDirs: this.state.refDirs,
					match: this.state.match
				}));
			} catch (e) { /* ignore */ }
		},

		/* ------------------------------------------------------ helpers */
		// Everything this window rendered outside itself. The search menu,
		// its form and the two date-picker menus render to document.body and
		// would outlive the window otherwise — one leaked set per open on a
		// desktop page that lives for days, with handlers still bound to a
		// destroyed window.
		onBeforeDestroy: function () {
			this.stopPolling();
			if (this.searchMenu) {
				try { this.searchMenu.destroy(); } catch (e) { /* already gone */ }
				this.searchMenu = null;
			}
			Ext.each(this._dateMenus || [], function (dm) {
				try { dm.destroy(); } catch (e) { /* already gone */ }
			});
			this._dateMenus = null;
		},
		fullPath: function (rec) {
			return (rec.get ? rec.get('path') : rec.path) + '/' + (rec.get ? rec.get('name') : rec.name);
		},
		// Toast: a bare styled element, NOT an Ext.Window — DSM's theme repaints
		// window chrome white, which would turn a window-based toast into a blank
		// box.
		/* Uses DSM's own SYNO.SDS.ToastBox, which renders into the owner
		   component — so the toast is scoped to this app's window rather than
		   floating over the whole desktop, the way DSM's own toasts behave.
		   text is injected as HTML by ToastBox, so it must stay escaped.
		   The hand-drawn .df-toast remains as a fallback for a DSM without
		   ToastBox, and for the pre-render case where there is no owner el to
		   align to — losing a "cannot be moved" message silently would be
		   worse than an unthemed one. */
		notify: function (msg) {
			this.dismissToast();
			var me = this;
			if (UxToast && this.rendered) {
				// Do NOT pass `listeners` in the config. ToastBox builds its own
				// listeners (afterrender -> defineToastBoxBehavior, which aligns
				// the box, schedules destroyBox.defer(delay) and calls
				// bindEvents() to wire the close button) and then does
				// Ext.apply(cfg, userCfg) — so a `listeners` key REPLACES the
				// whole object, leaving a toast that never auto-dismisses and
				// whose X does nothing. Attach after construction instead.
				this._toast = new UxToast({
					owner: this,
					text: esc(msg),
					delay: 3600,
					closable: true
				});
				this._toast.on('beforedestroy', function () { me._toast = null; });
				return;
			}
			var el = Ext.getBody().createChild({ tag: 'div', cls: 'df-toast', html: esc(msg) });
			this._toastEl = el;
			window.setTimeout(function () {
				if (me._toastEl === el) me._toastEl = null;
				el.remove();
			}, 3600);
		},

		dismissToast: function () {
			if (this._toast) {
				// beforedestroy nulls _toast; guard anyway in case it is
				// already destroyed with the window
				try { this._toast.destroy(); } catch (e) { /* already gone */ }
				this._toast = null;
			}
			if (this._toastEl) {
				this._toastEl.remove();
				this._toastEl = null;
			}
		},

		// App-masking wait dialog for operations that legitimately run long
		// (moves, exports). Indeterminate bar, no Stop button — File Station
		// is already executing, and there is nothing safe to abort from the
		// client. The mask also prevents a second overlapping request from
		// the same window. Returns the dialog; close() it when done.
		showBusy: function (title, text) {
			var bar = new Ext.ProgressBar({ height: 18, animate: false });
			// The count goes in its own line ABOVE the bar, never in the bar's
			// own text. Ext renders progress text inside the bar element, and
			// at the 18px height DSM's font needs it is clipped to a sliver.
			// The scan dialog keeps its label outside the bar for the same
			// reason; this matches it. A non-breaking space holds the line's
			// height so the dialog does not jump when the first update arrives.
			var progLine = new Ext.Panel({
				border: false,
				bodyStyle: 'padding:0 0 6px;font-size:' + FSMALL + ';color:' + FONT2 + ';',
				html: '&#160;'
			});
			// text is optional: an operation that reports its own progress
			// says everything it needs to in the title and the count line, and
			// an empty description panel would just add dead padding.
			var items = [];
			if (text) {
				items.push({ xtype: 'panel', border: false,
					bodyStyle: 'padding:0 0 9px;font-size:' + FSMALL + ';color:' + FONT2 + ';line-height:1.5;',
					html: esc(text) });
			}
			items.push(progLine, bar);
			var win = new UxModal({
				owner: this,
				title: title,
				width: 420, autoHeight: true, modal: false, closable: false, resizable: false,
				cls: 'df-app',
				bodyStyle: 'padding:16px;',
				items: items
			});
			win.show();
			// Indeterminate until a caller reports real progress: an operation
			// that cannot measure itself should not pretend to.
			bar.wait({ interval: 200 });
			// setProgress(done, total, label) switches the bar to a real
			// fraction the first time it is called. Ext's wait() animation
			// keeps running until reset(), and would otherwise fight every
			// updateProgress, so the first determinate update stops it.
			win.setProgress = function (done, total, label) {
				if (!total || bar.isDestroyed) return;
				if (!win._determinate) { win._determinate = true; bar.reset(); }
				// label carries a filename straight from the daemon and lands
				// in innerHTML: escape it.
				if (progLine.body) {
					progLine.body.update(esc(label || (done + ' of ' + total)));
				}
				bar.updateProgress(done / total, '', false);
			};
			return win;
		},

		/* ------------------------------------------------- build the UI */
		// The window's pieces, built once by the constructor. Each builder owns
		// one region and stores what it creates on `this`; the order matters
		// only where a later region embeds an earlier one: the grid takes the
		// selection model, store and pager, and the center panel takes both
		// toolbars, the idle panel and the grid.
		buildComponents: function () {
			this.buildRail();
			this.buildTopToolbar();
			this.buildGrid();
			this.buildBottomToolbar();
			this.buildCenterPanel();
		},

		// The left rail: the five tools with their badges, and the volume card
		// beneath them.
		buildRail: function () {
			var me = this;
			this.toolsStore = new Ext.data.JsonStore({
				fields: ['id', 'label', 'badge', 'ico'],
				data: TOOLS
			});
			this.toolsView = new Ext.DataView({
				store: this.toolsStore,
				flex: 1,
				autoScroll: true,
				overClass: 'x-grid3-row-over',   // DSM-themed hover
				tpl: new Ext.XTemplate(
					'<div style="padding-top:6px;">',
					'<tpl for=".">',
					// x-grid3-row-selected → DSM-themed selection highlight
					'<div class="df-rail-item {[values.id === this.sel() ? \'x-grid3-row-selected\' : \'\']}">',
					'{ico}<span class="df-rail-label">{label}</span>',
					'<span class="df-badge">{badge}</span></div>',
					'</tpl></div>',
					{ sel: function () { return me.state.tool; } }
				),
				itemSelector: 'div.df-rail-item',
				listeners: {
					scope: this,
					click: function (view, index) {
						this.setTool(TOOLS[index].id);
					}
				}
			});
			this.volPanel = new Ext.Panel({ border: false, height: 76, html: '' });
			this.westPanel = new Ext.Panel({
				region: 'west',
				width: 236,
				border: true,
				layout: 'vbox',
				layoutConfig: { align: 'stretch' },
				items: [this.toolsView, this.volPanel]
			});
		},

		// The top toolbar: Scan, Scope, the Match menu, the summary and DSM's
		// own search box.
		buildTopToolbar: function () {
			var me = this;
			this.scanBtn = new UxButton({
				btnStyle: 'blue',
				text: 'Scan',
				scope: this,
				handler: this.startScan
			});
			this.matchBtn = new UxButton({
				text: 'Match: ' + this.matchLabel(),
				menu: new UxMenu({
					items: [
						{ text: 'Content (' + this.state.hashAlgo + ' hash) — required', checked: true, disabled: true, hideOnClick: false, checkHandler: Ext.emptyFn },
						'-',
						this.matchItem('name', 'File Name'),
						this.matchItem('modified', 'Modified Date'),
						this.matchItem('created', 'Created Date')
					]
				})
			});
			this.summaryText = new Ext.Toolbar.TextItem('');
			/* DSM's own search box (a TriggerField with the magnifier). Note
			   emptyText is deliberately NOT set: SearchField's constructor
			   installs the LOCALIZED "Search" placeholder, and passing our own
			   would overwrite it with English. Falls back to a plain text
			   field on a DSM without SearchField. */
			var searchCfg = {
				width: 170,
				enableKeyEvents: true,
				listeners: {
					scope: this,
					keyup: { buffer: 250, fn: function (f) {
						this.state.query = f.getValue();
						this.applySearch();
					} }
				}
			};
			if (UxSearch) {
				// The clear (X) trigger calls setValue('') + this.filter()
				// WITHOUT any key event, so keyup-only wiring would leave
				// state.query stale: clearing the box would keep showing the old
				// filtered page. filter() is TextFilter's one funnel for
				// programmatic value changes, and with no store configured its
				// stock body is a no-op — overriding it per-instance is the
				// designed hook, not a clobbered constructor key.
				searchCfg.filter = function () {
					me.state.query = this.getValue();
					// X wipes the whole search, options included — File
					// Station's X does the same; leaving hidden menu filters
					// active behind an empty box would look like lost results
					if (!me.state.query) me.resetSearchOpts();
					me.applySearch();
				};
				// the magnifier opens the options menu (File Station's
				// advanced search); getMenu is SearchField's designed hook —
				// its prototype default is an empty function
				searchCfg.getMenu = function () { return me.getSearchMenu(); };
			}
			// The key must be ABSENT, not undefined: Ext copies undefined-valued
			// config keys straight onto the instance, so `emptyText: undefined`
			// would wipe SearchField's localized placeholder rather than leave
			// it alone.
			if (!UxSearch) searchCfg.emptyText = 'Search';
			this.searchField = new (UxSearch || UxText)(searchCfg);
			this.topToolbar = new UxToolbar({
				items: [
					this.scanBtn, '-',
					{ text: 'Scope…', scope: this, handler: this.openScopeDialog },
					'-',
					this.matchBtn,
					'->',
					this.summaryText, ' ',
					this.searchField
				]
			});
			// The summary's issues link opens the full list (openIssuesDialog).
			// The summary is re-rendered on every refresh, so the handler is
			// delegated from the toolbar rather than bound to the link itself.
			this.topToolbar.on('afterrender', function (tb) {
				tb.getEl().on('click', function (ev) {
					ev.preventDefault();
					me.openIssuesDialog(me.state.tool);
				}, me, { delegate: 'a.df-issues' });
			}, this, { single: true });
		},

		// The results grid: the checkbox selection model with its padlock
		// renderer, the paging store, DSM's pager, the grouping view and the
		// grid that carries them.
		buildGrid: function () {
			this.buildSelectionModel();
			this.buildStore();
			this.buildPager();
			this.grid = new UxGrid({
				border: false,
				store: this.store,
				sm: this.sm,
				// the pager belongs to the grid, directly under the rows —
				// where File Station puts it
				bbar: this.pager,
				// Every page turn is a round trip. Without the mask the grid
				// would show its "No results" text while one is in flight,
				// which on a big result set reads as "nothing found".
				loadMask: true,
				enableColumnHide: true,
				columns: [
					this.sm,
					// GroupingView needs the grouped field present in the column
					// model; keep it permanently hidden.
					{ header: 'Group', dataIndex: 'gidx', hidden: true, hideable: false, width: 10 },
					{ id: 'name', header: 'Name', dataIndex: 'name', width: 240, sortable: true, renderer: function (v, m, r) {
						// Second explanation for the same fact, and deliberately
						// so: the padlock carries the full sentence on hover,
						// but the eye lands on the name, not on a 20px glyph.
						if (r.get('prot')) m.attr = 'ext:qtip="Read-only reference"';
						return esc(v);
					} },
					// Conflicting Files only, hidden everywhere else (setToolColumns).
					// Status sits beside the name because on that tool it is the
					// column the whole listing exists to show.
					// fixed: forceFit scales every other column to fill the grid, and
					// with this tool's two extra columns showing it would squeeze Status
					// to 57px — narrower than the word it has to render (measured
					// on-device; jsdom does no layout).
				{ header: 'Status', dataIndex: 'verdict', width: 110, fixed: true, sortable: false, hidden: true, renderer: function (v, m, r) {
						if (!v) return '';
						var ev = r.get('evidence');
						if (ev) m.attr = 'ext:qtip="' + qtipText(ev) + '"';
						return '<span class="df-verdict df-verdict-' + esc(v) + '">' + esc(VERDICT_TEXT[v] || v) + '</span>';
					} },
					{ header: 'Evidence', dataIndex: 'evidence', width: 300, sortable: false, hidden: true, renderer: function (v) {
						if (!v) return '<span class="df-dim">no evidence either way</span>';
						return '<span ext:qtip="' + qtipText(v) + '">' + esc(v) + '</span>';
					} },
					{ header: 'Location', dataIndex: 'path', width: 260, sortable: true, renderer: function (v) {
						return '<span class="df-loc" ext:qtip="Open in File Station">' + esc(v) + '</span>';
					} },
					{ header: 'Size', dataIndex: 'size', width: 90, align: 'right', sortable: true, renderer: function (v, m, r) {
						return r.get('isDir') ? 'Empty' : fmtBytes(v);
					} },
					{ header: 'Created Date', dataIndex: 'created', width: 160, sortable: true, hidden: false, renderer: fmtDate },
					{ header: 'Modified Date', dataIndex: 'date', width: 160, sortable: true, renderer: fmtDate },
					{ header: 'Captured Date', dataIndex: 'captured', width: 160, sortable: true, hidden: false, renderer: fmtDate },
					{ header: 'Hash', dataIndex: 'hash', width: 150, sortable: false, renderer: function (v) {
						return '<span class="df-mono">' + esc(v || '—') + '</span>';
					} }
				],
				view: this.buildGridView(),
				listeners: {
					scope: this,
					// re-apply the dynamic checker states and the manual sort
					// arrow after any full re-render wipes them
					viewready: function (g) {
						g.getView().on('refresh', this.updateCheckerStates, this);
						g.getView().on('refresh', this.updateFlatSortIcon, this);
					},
					rowclick: function (grid, idx, e) {
						if (e.getTarget('.x-grid3-row-checker')) return;
						if (e.getTarget('.df-loc')) {
							this.openFileStation(grid.getStore().getAt(idx).get('path'));
							return;
						}
						var rec = grid.getStore().getAt(idx);
						if (rec.get('prot')) {
							// silent: the padlock and its two tooltips already say
							// this, so a toast just repeated it
							return;
						}
						if (this.sm.isSelected(idx)) this.sm.deselectRow(idx);
						else this.sm.selectRow(idx, true);
					}
				}
			});
		},

		// The checkbox column. Its renderer is where the padlock for a read-only
		// reference row and the disabled box for an unaddressable file come from,
		// and beforerowselect is the one chokepoint every selection path passes.
		buildSelectionModel: function () {
			this.sm = new Ext.grid.CheckboxSelectionModel({
				// checkOnly disables the model's own mousedown row-select, which
				// would fire before rowclick can tell a Location link was hit
				// (checking the box on every link click). Checkbox clicks are
				// still handled natively; the grid's rowclick handler below owns
				// row-body toggling.
				checkOnly: true,
				width: 32, // 20px sprite + the cell's 8px left padding
				/* The checker's className must stay EXACTLY
				   'x-grid3-row-checker': Ext's CheckboxSelectionModel does
				   `t.className == "x-grid3-row-checker"` — string equality, not
				   a class-list test — so adding any second class silently stops
				   every checkbox click from toggling selection. (DSM's checkbox
				   art is applied by URL from the stylesheet instead — see
				   dsmCheckboxSprite.)
				   This renderer therefore never DECORATES the checker; on a
				   read-only reference row it emits a padlock INSTEAD of one,
				   leaving every other row's checker byte-identical to stock.
				   A protected row is not a candidate whose box is switched off,
				   it is a row with no box to switch — and DSM has no shared
				   icon class for that. `syno-ux-status-shield-disabled` does
				   NOT exist: checked live on DSM 7.4 across all 67 stylesheets
				   the desktop loads, and there is no `shield` selector in any
				   of them. The only protection art on the box is app-private
				   PNGs inside HyperBackupVault and ScsiTarget, which a package
				   must not reach into.
				   The glyph is therefore the Unicode padlock U+1F512 (written
				   as a numeric entity so the meaning survives any re-encoding
				   of this file), NOT drawn art, which also avoids an SVG
				   hazard: an SVG element's .className is an SVGAnimatedString,
				   not a string, and Ext's event plumbing (getTarget, QuickTips)
				   assumes a string, so an SVG node must never become an event
				   target. A text node in a plain div cannot be one. */
				renderer: function (v, p, record) {
					if (record && record.get('prot')) {
						return '<div class="df-prot-lock" ext:qtip="Read-only reference file — cannot be moved">' +
							'&#128274;</div>';
					}
					/* DSM cannot see this name through File Station, so no move
					   could ever succeed (see fsCannotAddress on the daemon).
					   The DISABLED checkbox is the same thing the last-copy
					   case shows, because it says the same thing — and unlike
					   the padlock this row DOES keep a box, so the sprite lands
					   on the checker itself. qtip on the inner div, not the
					   cell: updateCheckerStates clears the CELL's qtip on every
					   pass, and this reason never changes. */
					if (record && record.get('nomove')) {
						// className stays EXACTLY stock (see the CSS note); the
						// disabled sprite arrives from the row class getRowClass
						// adds. ext:qtip is an attribute, not a class, so it is
						// safe here — and it belongs on this div rather than the
						// cell, which updateCheckerStates clears on every pass.
						return '<div class="x-grid3-row-checker" ' +
							'ext:qtip="DSM cannot see this file, so it cannot be moved — ' +
							'delete it from the Mac that made it, or move the folder that holds it">&#160;</div>';
					}
					return '<div class="x-grid3-row-checker">&#160;</div>';
				},
				listeners: {
					scope: this,
					beforerowselect: function (sm, idx, keep, rec) {
						// A read-only tool's rows are a report. This is the one
						// chokepoint every selection path funnels through —
						// row clicks, the Select menu, bulk selects — so the
						// veto belongs here rather than at each caller.
						if (isReadOnly(this.state.tool)) return false;
						if (rec.get('prot')) return false;
						// No move can succeed for it, so selecting it could
						// only ever produce a refusal in the results dialog.
						if (rec.get('nomove')) return false;
						// never let a duplicate group be selected whole — at
						// least one copy must survive the move
						if (!this.groupHasUnselectedPeer(rec)) {
							// Refused silently: the disabled checkbox and the tooltip
							// updateCheckerStates puts on it already explain why,
							// so a toast on top of that is redundant noise.
							return false;
						}
						return true;
					},
					selectionchange: this.updateSelectionUI
				}
			});
		},

		// The paging store behind the grid, driven by the daemon's page API, with
		// header-click sorting routed to the daemon for the flat tools.
		buildStore: function () {
			var me = this;
			// paramNames.start is 'offset': DSM's own paging stores use
			// offset/limit, and so does the daemon's results API — so the
			// PagingToolbar's page buttons request pages directly, with no
			// translation layer.
			this.store = new Ext.data.GroupingStore({
				proxy: new DfPagingProxy({ win: this }),
				reader: new Ext.data.JsonReader({ idProperty: 'id', root: 'rows', totalProperty: 'total' }, [
					// An explicit field list: a JSON key that is not named here is
					// dropped silently, and its column then renders blank —
					// which reads exactly like a backend bug.
					'id', 'name', 'path', 'size', 'date', 'created', 'captured',
					'hash', 'ext', 'prot', 'isDir', 'gidx', 'grpLabel', 'ord',
					'verdict', 'evidence', 'nomove'
				]),
				paramNames: { start: 'offset', limit: 'limit', sort: 'sort', dir: 'dir' },
				groupField: 'gidx',
				sortInfo: { field: 'ord', direction: 'ASC' },
				// The daemon sorts (see onFlatSort); remoteSort here would make
				// Store.sort reload with its own sort/dir params instead.
				remoteSort: false,
				listeners: { scope: this, load: this.onPageLoaded }
			});
			// Flat tools sort server-side: the store holds one page, so
			// sorting it in place would silently mis-order it against the
			// pages around it. Header clicks funnel through
			// store.sort(dataIndex) — intercept them there and re-request
			// from page one in the new order. Grouping calls (arrays), no-arg
			// re-sorts and the 'ord' arrival order pass through to the local
			// sort; duplicates always sort locally (whole groups are on the
			// page, so within-group sorting is sound).
			var origStoreSort = this.store.sort;
			this.store.sort = function (field, dir) {
				if (!isGrouped(me.state.tool) && typeof field === 'string' &&
						field !== 'ord' && field !== 'gidx') {
					me.onFlatSort(field, dir);
					return true;
				}
				return origStoreSort.apply(this, arguments);
			};
		},

		// DSM's own paging toolbar, counting in the unit the tool pages in.
		buildPager: function () {
			// DSM's own pager: numbered pages, ±5-page jumps, first/last, the
			// item count and a refresh button — the same control File Station
			// puts under a long folder listing. It hides its own navigation
			// when everything fits on one page.
			var PagerClass = SYNO.ux.PagingToolbar || Ext.PagingToolbar;
			this.pager = new PagerClass({
				store: this.store,
				pageSize: PAGE_GROUPS,
				displayInfo: true,
				displayMsg: '{2} items',
				emptyMsg: 'No results'
			});
			// DSM decides whether to show its page navigation by comparing the
			// whole-set total against the number of loaded records. That holds
			// when both count rows — but the duplicates view pages in GROUPS,
			// so the total (groups) is smaller than the record count (rows)
			// and the navigation would hide itself on exactly the multi-page
			// listings that need it. The page count is the real criterion, in
			// either unit.
			if (this.pager.setButtonsVisible) {
				var basePager = PagerClass.prototype;
				this.pager.setButtonsVisible = function () {
					basePager.setButtonsVisible.call(this, this.getTotalPage() > 1);
				};
			}
		},

		// The grouping view, built through DSM's FleXcroll factory when the
		// desktop provides it and on the stock view otherwise.
		buildGridView: function () {
			// Grid view: DSM ships SYNO.SDS.DefineGridView, the factory it
			// uses to build its own FleXcroll grid views (overlay scrollbar,
			// like File Station). Synology never made a grouped variant, but
			// the factory wraps any base class — so ask it for one built on
			// GroupingView. If the factory is missing (dev harness, future
			// DSM), fall back to the stock view with styled native bars.
			var ViewClass = Ext.grid.GroupingView;
			try {
				if (SYNO.SDS.DefineGridView) {
					if (!SYNO.SDS.DuplicateFinder.FleXGroupingView) {
						SYNO.SDS.DefineGridView('SYNO.SDS.DuplicateFinder.FleXGroupingView', 'Ext.grid.GroupingView');
					}
					if (SYNO.SDS.DuplicateFinder.FleXGroupingView) {
						ViewClass = SYNO.SDS.DuplicateFinder.FleXGroupingView;
					}
				}
			} catch (e) { ViewClass = Ext.grid.GroupingView; }
			var viewCfg = {
				// File Station-style column sizing: columns scale
				// proportionally to always fill the grid width, re-fitting
				// on window resize and when columns are shown/hidden.
				forceFit: true,
				// df-row-prot marks a read-only reference row. It paints
				// nothing — protection is signalled by the padlock the selection
				// model's renderer puts where the checkbox would be, not by the
				// row's text colour — but the class stays as the row-level hook
				// that says which rows those are.
				getRowClass: function (rec) {
					// Composed, not returned early: a row can be both
					// unmovable and verdict-tinted, and getRowClass gets one
					// string for all of it.
					var cls = rec.get('nomove') ? 'df-row-nomove' : '';
					if (rec.get('prot')) return cls ? cls + ' df-row-prot' : 'df-row-prot';
					// A corrupted row is tinted so the damaged copy is findable
					// by eye down a long listing, not only by reading the
					// Status column.
					var v = rec.get('verdict');
					if (v === 'corrupt') return cls ? cls + ' df-row-corrupt' : 'df-row-corrupt';
					return cls;
				},
				showGroupName: false,
				enableGroupingMenu: false,
				enableNoGroups: false,
				hideGroupedColumn: false,
				groupTextTpl: '<span class="df-group-label">{[values.rs[0] ? values.rs[0].data.grpLabel : ""]}</span>',
				emptyText: 'No results'
			};
			if (ViewClass === Ext.grid.GroupingView) {
				// classic (space-taking) scrollbars in the fallback: Ext
				// measures scrollbar width on a plain element — 0px on macOS
				// overlay bars — while the styled bars take a real 20px.
				// Reserve it or the grid overflows sideways. The FleXcroll
				// view is an overlay (factory default scrollOffset:0): its
				// thumb floats over the row-line ends, like File Station.
				viewCfg.scrollOffset = 20;
			}
			return new ViewClass(viewCfg);
		},

		// The bottom toolbar: the selection summary, Select, Move to… and
		// Export.
		buildBottomToolbar: function () {
			this.selText = new Ext.Toolbar.TextItem('Select files to act on them');
			this.moveBtn = new UxButton({
				btnStyle: 'blue',
				text: 'Move to…',
				disabled: true,
				scope: this,
				handler: this.openMoveDialog
			});
			this.exportBtn = new UxButton({
				text: 'Export',
				scope: this,
				handler: this.openExportDialog
			});
			this.selectMenuBtn = new UxButton({
				text: 'Select',
				menu: new UxMenu({
					items: [
						{ text: 'Keep Newest in Each Group', itemId: 'selNewest', scope: this, handler: function () { this.autoSelect('newest'); } },
						{ text: 'Keep Oldest in Each Group', itemId: 'selOldest', scope: this, handler: function () { this.autoSelect('oldest'); } },
						{ text: 'Select Files in Folder…', scope: this, handler: this.openDirSelectDialog },
						'-',
						{ text: 'Deselect All', scope: this, handler: function () { this.sm.clearSelections(); } }
					]
				})
			});
			this.bottomToolbar = new UxToolbar({
				items: [this.selText, '->', this.selectMenuBtn, this.moveBtn, this.exportBtn]
			});
		},

		// The center card panel, switching between the idle text and the grid,
		// with both toolbars.
		buildCenterPanel: function () {
			this.idlePanel = new Ext.Panel({
				border: false,
				html: '',
				autoScroll: true
			});

			this.centerPanel = new Ext.Panel({
				region: 'center',
				layout: 'card',
				activeItem: 0,
				border: false,
				// Synology insets content by padding the container region
				// (File Station: padding on its gridpanel wrapper), not by
				// shrinking pieces inside it — the grid's header, column
				// selector, rows and the fleXcroll anchor all move together,
				// so the thumb overlaps the row-line ends like File Station.
				bodyStyle: 'padding-right:12px;',
				tbar: this.topToolbar,
				bbar: this.bottomToolbar,
				items: [this.idlePanel, this.grid]
			});
		},

		matchItem: function (key, label) {
			var me = this;
			return {
				text: label,
				checked: !!this.state.match[key],
				hideOnClick: false,
				checkHandler: function (item, checked) {
					me.onMatchToggled(key, checked, label);
				}
			};
		},

		// Match refinement is applied by the daemon (the loaded window is only
		// part of the result set), so a toggle re-requests the first page.
		onMatchToggled: function (key, checked, label) {
			this.state.match[key] = checked;
			this.savePrefs();
			this.matchBtn.setText('Match: ' + this.matchLabel());
			if (this.state.tool !== 'duplicates' || !this.state.scanned.duplicates) return;
			// unchecking a criterion the scan applied cannot widen the
			// results — the scan skipped hashing across buckets
			var res = this.state.results.duplicates;
			if (!checked && res && res.scanMatch && res.scanMatch[key]) {
				this.notify('The last scan only compared files matching on ' +
					label.toLowerCase() + ' — rescan to find duplicates without that requirement');
			}
			// refinement changes how many groups there are — re-page from one
			this.loadPage(0);
		},
		matchLabel: function () {
			var l = [this.state.hashAlgo || 'Blake3'];
			if (this.state.match.name) l.push('Name');
			if (this.state.match.modified) l.push('Modified');
			if (this.state.match.created) l.push('Created');
			return l.join(' + ');
		},

		/* ---------------------------------------------------- bootstrap */
		bootstrap: function () {
			var me = this;
			api('/info', 'GET', null, function (err, j) {
				if (err) {
					me.notify(err);
					me.refreshView();
					return;
				}
				me.state.volumes = j.volumes || [];
				me.state.hashAlgo = j.hashAlgo || 'Blake3';
				// the account to grant shared-folder access to; it differs
				// between builds, so the daemon reports it (see serviceUser)
				me.state.serviceUser = j.user || '';
				VOLUMES = me.state.volumes;
				// an empty picker with a warning is a broken session/API, not
				// "no volumes" — say so instead of silently showing nothing
				if (j.warning && !me.state.volumes.length) {
					me.notify('Volume list unavailable: ' + j.warning);
				}
				if (!me.state.dirs.length && me.state.volumes.length) {
					var v0 = me.state.volumes[0];
					Ext.each((v0.shares || []).slice(0, 6), function (s) {
						if (s && s.path) me.state.dirs.push(s.path);
					});
				}
				me.renderVolumes();
				api('/state', 'GET', null, function (err2, st) {
					if (err2) {
						me.notify(err2);
						me.refreshView();
						return;
					}
					// The daemon's list is the one the STORED results were scanned
					// with: it decides the padlocks and the move's refusals. The
					// Scope dialog edits a separate list for the NEXT scan.
					me.state.scanRefDirs = st.refDirs || [];
					me.noteSaveError(st);
					if (st.refDirs && st.refDirs.length && !me.state.refDirs.length) {
						me.state.refDirs = st.refDirs;
					}
					// A scan the previous daemon run never finished (restart,
					// reboot, crash). Held rather than acted on: the next
					// Scan click offers the real choice — Resume (continue the
					// dead run, its finished reads are not repeated) or Start
					// Over — because "resume" must never be something the app
					// silently decides. The daemon clears the notice when any
					// scan starts, so the offer naturally expires with it.
					if (st.interrupted && st.interrupted.tool && TOOL_NAME[st.interrupted.tool]) {
						me._interrupted = st.interrupted;
						me.notify('The last ' + TOOL_NAME[st.interrupted.tool].toLowerCase() +
							' scan was interrupted by a restart' +
							(st.interrupted.tool === 'duplicates'
								? ' — the next Scan will offer to resume it' : ' — run Scan again') + '.');
					}
					var tools = [];
					var k;
					for (k in (st.scanned || {})) {
						if (st.scanned.hasOwnProperty(k)) tools.push(k);
					}
					var pending = tools.length;
					if (st.running) me.attachProgress();
					if (!pending) {
						me.refreshView();
						return;
					}
					// Every scanned tool's totals feed its rail badge; only the
					// visible one loads an actual page of rows.
					Ext.each(tools, function (tool) {
						me.fetchMeta(tool, function () {
							pending--;
							if (!pending) {
								me.refreshView();
								if (me.state.scanned[me.state.tool]) me.loadPage(0);
							}
						});
					});
				});
			});
		},

		renderVolumes: function () {
			var v = this.state.volumes[0];
			if (!v) {
				this.volPanel.update('');
				return;
			}
			var pct = v.totalBytes ? Math.round(100 * v.usedBytes / v.totalBytes) : 0;
			this.volPanel.update(
				'<div class="df-vol"><b>' + esc(v.name) + '</b>' +
				(VOL_BAR ? VOL_BAR.fill(pct)
					: '<div class="df-vol-bar"><div class="df-vol-fill" style="width:' + pct + '%"></div></div>') +
				fmtCapacity(v.usedBytes) + ' of ' + fmtCapacity(v.totalBytes) + ' used</div>'
			);
		},

		/* ------------------------------------------------- tool switching */
		setTool: function (id) {
			this.state.tool = id;
			this.sm.clearSelections();
			this.refreshView();
			// each tool pages independently: show its first page
			if (this.state.scanned[id]) this.loadPage(0);
		},

		/* ------------------------------------------------- paged results */
		pageSize: function (tool) {
			return isGrouped(tool) ? PAGE_GROUPS : PAGE_FILES;
		},

		// The pager counts groups for duplicates (the daemon pages whole
		// groups) and rows everywhere else, so its page size and the unit in
		// its count must follow the tool on display. The count text is set
		// here rather than left to DSM's own plural handling, because that
		// only ever says "items" — this listing also counts groups.
		configurePager: function (tool, total) {
			this.pager.pageSize = this.pageSize(tool);
			var one = total === 1;
			var unit = one ? 'item' : 'items';
			if (tool === 'duplicates') unit = one ? 'group' : 'groups';
			else if (tool === 'corrupted_files') unit = one ? 'set' : 'sets';
			this.pager.displayMsg = '{2} ' + unit;
		},

		// Loads one page into the grid. offset counts groups for duplicates,
		// rows otherwise — the same unit the daemon pages in and the pager
		// counts in, so the pager's own buttons can call straight through.
		loadPage: function (offset) {
			var t = this.state.tool;
			if (!this.state.scanned[t]) return;
			this.configurePager(t);
			this.store.load({ params: { offset: Math.max(0, offset || 0), limit: this.pageSize(t) } });
		},

		// How much of the pager's unit is on the current page: groups for
		// duplicates (compare against store.getTotalCount(), which counts
		// groups too), rows for the flat tools.
		pageUnitCount: function () {
			if (!isGrouped(this.state.tool)) return this.store.getCount();
			var seen = {}, n = 0;
			this.store.each(function (r) {
				var g = r.get('gidx');
				if (!seen[g]) { seen[g] = true; n++; }
			});
			return n;
		},

		// Reloads whatever page is on screen (after a move, say). Ext keeps
		// the last request's params, so this re-asks for the same window.
		reloadPage: function () {
			var t = this.state.tool;
			if (!this.state.scanned[t]) return;
			var last = this.store.lastOptions && this.store.lastOptions.params;
			this.loadPage(last ? last.offset || 0 : 0);
		},

		// The display parameters a fetched page depends on. The daemon applies
		// search, match refinement and sorting so that totals and row order
		// stay correct across the whole result set, not just the page on
		// screen. Reference folders are NOT sent: protection (padlocks and
		// the reclaimable totals) is decided by the daemon from the folders
		// the stored results were scanned with, so it cannot disagree with
		// the refusals the move issues.
		fetchParams: function (tool) {
			var p = {
				q: String(this.state.query || '').replace(/^\s+|\s+$/g, '')
			};
			// magnifier-menu options ride along with q; the daemon ANDs them
			// and keeps duplicate groups whole (any-member-match, like q)
			var so = this.state.searchOpts || {};
			if (so.loc) p.loc = so.loc;
			if (so.ftype) {
				p.ftype = so.ftype;
				if (so.ftype === 'ext') p.extq = so.extq || '';
			}
			if ((so.dateFrom || so.dateTo) && so.dateBy) {
				p.dateBy = so.dateBy;
				if (so.dateFrom) p.dateFrom = so.dateFrom;
				if (so.dateTo) p.dateTo = so.dateTo;
			}
			if (so.sizeOp) {
				p.sizeOp = so.sizeOp;
				p.sizeMB = so.sizeMB || 0;
			}
			if (tool === 'duplicates') {
				p.match = {
					name: !!this.state.match.name,
					modified: !!this.state.match.modified,
					created: !!this.state.match.created
				};
			}
			// Grouped views have no server-side row sort (the daemon returns
			// whole sets in its own listing order), so sending sort params
			// would round-trip and come back identically ordered while the
			// header painted an arrow that lies about it.
			var srt = this._flatSort[tool];
			if (srt && !isGrouped(tool)) {
				p.sort = srt.field;
				p.dir = srt.dir;
			}
			return p;
		},

		// Two columns exist only for Conflicting Files. Every other column is
		// left exactly as the user set it — enableColumnHide is on, and
		// re-asserting a whole layout on each tool switch would silently undo
		// their choices.
		setToolColumns: function (tool) {
			var cm = this.grid && this.grid.getColumnModel();
			if (!cm) return;
			var show = tool === 'corrupted_files';
			Ext.each(['verdict', 'evidence'], function (di) {
				var i = cm.findColumnIndex(di);
				if (i >= 0 && cm.isHidden(i) === show) cm.setHidden(i, !show);
			});
			// Captured Date is read from photo EXIF and says nothing about
			// corruption; on this tool its width is better spent on Evidence,
			// which is otherwise squeezed to 145px (measured on-device).
			var ci = cm.findColumnIndex('captured');
			if (ci >= 0 && cm.isHidden(ci) !== show) cm.setHidden(ci, show);
			// The checker column goes with the ability to select: a row of
			// checkboxes that silently refuse every click is worse than none.
			// It is column 0 — the selection model itself.
			var ro = isReadOnly(tool);
			if (cm.isHidden(0) !== ro) cm.setHidden(0, ro);
		},

		refreshView: function () {
			var t = this.state.tool;
			this.toolsView.refresh();
			this.matchBtn.setVisible(t === 'duplicates');
			this.setToolColumns(t);
			// A read-only tool reports; it never acts. The scan it offers is
			// the one that fills it in.
			var ro = isReadOnly(t);
			this.selectMenuBtn.setVisible(!ro);
			this.moveBtn.setVisible(!ro);
			this.scanBtn.setText(this.state.scanned[scanToolFor(t)] ? 'Rescan' : 'Scan');

			if (this.state.scanned[t]) {
				this.configurePager(t);
				this.summaryFor(t);
				this.centerPanel.getLayout().setActiveItem(1);
			} else {
				this.idlePanel.update(
					'<div class="df-idle"><h2>Scan for ' + esc(TOOL_NAME[t].toLowerCase()) + '</h2>' +
					'<p>' + esc(IDLE_DESC[t]) + '</p>' +
					'<p style="color:' + FONT3 + ';">Scope: ' + esc(this.state.dirs.length +
						(this.state.dirs.length === 1 ? ' folder' : ' folders')) + ' — use the toolbar to scan' +
					(isReadOnly(t) ? ', which runs the ' + esc(TOOL_NAME[scanToolFor(t)]) + ' scan' : '') + '.</p></div>'
				);
				this.summaryText.setText('');
				this.centerPanel.getLayout().setActiveItem(0);
			}
			this.updateSelectionUI();
			this.updateBadges();
			this.topToolbar.doLayout();
		},

		/* --------------------------------------------------- row building */
		// The one place a fetched page's whole-set metadata is recorded. If
		// pageToRows and fetchMeta each carried this literal, a field added to
		// one would silently go missing from the other path. scanned is
		// written here and only here.
		noteResultMeta: function (tool, j) {
			var total = j.total || {};
			this.state.results[tool] = {
				errors: j.errors || [], scanMatch: j.match || null,
				total: total, grand: j.grand || total,
				truncated: j.truncated || null,
				dateRange: j.dateRange || null
			};
			this.state.scanned[tool] = true;
		},

		// Group headers. Every value interpolated is escaped: the label is
		// raw HTML handed to groupTextTpl, and a filename reaches it through
		// g.ext. g.count is set when the page trimmed the group's rows: the
		// header must describe the WHOLE group, and say so, rather than the
		// handful of rows that fit on the page.
		corruptedGroupLabel: function (g, shown, whole) {
			return fmtNum(whole) + ' files claim to be the same file &nbsp;·&nbsp; ' +
				fmtBytes(g.size) + ' each &nbsp;·&nbsp; ' + esc(g.mod || '') +
				' &nbsp;—&nbsp; <span class="df-group-waste">' + fmtNum(g.variants || 2) +
				' different contents</span>' +
				(g.sameName ? '' : ' &nbsp;·&nbsp; <span class="df-group-more">different names</span>') +
				(whole > shown ? ' &nbsp;·&nbsp; <span class="df-group-more">showing the first ' +
					fmtNum(shown) + '</span>' : '');
		},
		dupGroupLabel: function (g, shown, whole) {
			// g.prot is the daemon's protected count over the WHOLE group. On
			// a trimmed group the rows on the page are a subset, so counting
			// from them and pairing it with `whole` would overstate the
			// reclaimable figure — five protected copies off the page would
			// read as one kept.
			var keep = Math.max(g.prot || 0, 1);
			var recl = g.size * Math.max(0, whole - keep);
			return fmtNum(whole) + ' identical files &nbsp;·&nbsp; ' + fmtBytes(g.size) + ' each &nbsp;·&nbsp; ' + esc(g.ext) +
				' &nbsp;—&nbsp; <span class="df-group-waste">' + fmtBytes(recl) + ' reclaimable</span>' +
				(whole > shown ? ' &nbsp;·&nbsp; <span class="df-group-more">showing the first ' +
					fmtNum(shown) + '</span>' : '');
		},

		// Turns one page of the daemon's JSON into grid rows (called by the
		// store's proxy). Duplicate groups become a run of rows sharing a
		// group key so the grouped view draws one header per group; flat
		// tools map straight across. The page's whole-set metadata is kept
		// for the summary, the badges and the truncation notice. The returned
		// total is in the pager's unit: groups for duplicates, rows otherwise.
		pageToRows: function (tool, j) {
			var me = this, rows = [], ord = 0, grpIds = {};
			var total = j.total || {};
			this.noteResultMeta(tool, j);
			if (isGrouped(tool)) {
				Ext.each(j.groups || [], function (g, gi) {
					var shown = g.files.length;
					var whole = g.count || shown;
					var label = tool === 'corrupted_files' ? me.corruptedGroupLabel(g, shown, whole) : me.dupGroupLabel(g, shown, whole);
					// Keyed by TOOL plus the group's ABSOLUTE position in the
					// result set — never its slot on the page. Ext's GroupingView
					// keeps per-group collapse state under this value and clears
					// it on nothing: not a reload, a page turn or a tool switch.
					// A per-page index would therefore make collapsing the 4th
					// group on page 1 silently collapse whatever is 4th on page 2,
					// and those hidden rows stay selectable and movable.
					//
					// The tool prefix matters as much: both grouped tools page from
					// offset 0, so absolute position ALONE would still collide
					// across them, and a collapse on Duplicates would hide an
					// unrelated set of Conflicting Files. The prefix is constant
					// within a tool, so the group ordering GroupingStore derives
					// from this string is unaffected.
					var gKey = tool + ':' + pad6((j.offset || 0) + gi);
					grpIds[gKey] = [];
					Ext.each(g.files, function (f) {
						grpIds[gKey].push(f.id);
						rows.push(Ext.apply({}, f, {
							// prot comes from the daemon, decided on canonical
							// paths against the reference folders the results
							// were scanned with — the same comparison the move
							// refuses with. Conflicting Files rows are never
							// selectable at all (the veto lives in
							// beforerowselect), so nothing there is "protected"
							// in the reference-folder sense.
							prot: tool === 'duplicates' && !!f.prot,
							gidx: gKey, grpLabel: label, ord: ord++
						}));
					});
				});
			} else {
				Ext.each(j.files || [], function (f) {
					rows.push(Ext.apply({}, f, {
						prot: !!f.prot,
						gidx: '', grpLabel: '', ord: ord++
					}));
				});
			}
			// ids per duplicate group — keeps the keep-one-per-group checks
			// O(group size) instead of a full store scan per row
			this._grpIds = grpIds;
			return {
				rows: rows,
				total: isGrouped(tool) ? (total.groups || 0) : (total.files || 0)
			};
		},

		// Runs after every page load: grouping mode, summary, badges and the
		// selection UI all follow the page that just arrived. Paging replaces
		// the store's records, so the selection starts empty on each page —
		// the same as changing folder in File Station.
		onPageLoaded: function () {
			var t = this.state.tool;
			// A move prunes rows, and reloadPage re-asks for the SAME offset —
			// which can now sit past the end of the shrunken set. The daemon
			// clamps the offset up to the length and correctly returns an
			// empty page, so the grid falls back to "No results" while the
			// summary still reports the whole set, and the pager hides its
			// navigation (one page now), leaving no control to page back
			// with. Land on the last page that still exists. The retry offset
			// is strictly smaller, so this cannot loop.
			var lastP = this.store.lastOptions && this.store.lastOptions.params;
			var askedAt = lastP ? (lastP.offset || 0) : 0;
			var totalUnits = this.store.getTotalCount();
			if (askedAt > 0 && totalUnits > 0 && this.store.getCount() === 0) {
				var per = this.pageSize(t);
				this.loadPage((Math.ceil(totalUnits / per) - 1) * per);
				return;
			}
			// runs before the pager's own load handler, so the count text it
			// is about to paint is already right for this result set
			this.configurePager(t, this.store.getTotalCount());
			if (isGrouped(t)) this.store.groupBy('gidx');
			else {
				// A header click on a GROUPED tool falls through to the stock
				// GroupingStore.sort, which leaves store.sortInfo pointing at
				// that column for good. clearGrouping() below re-sorts from
				// sortInfo, so without this reset a flat tool's page — which
				// the daemon already returned in whole-set order — gets
				// re-sorted locally by whatever column was last clicked on
				// Duplicates, and the header paints an arrow claiming a
				// whole-set sort the pages around it do not share. 'ord' is
				// the arrival order the daemon sent and is not a column, so
				// restoring it repaints nothing.
				this.store.sortInfo = { field: 'ord', direction: 'ASC' };
				this.store.clearGrouping();
			}
			this.summaryFor(t);
			this.updateFlatSortIcon();
			this.updateSelectionUI();
			this.updateBadges();
		},

		summaryFor: function (t) {
			if (t === 'duplicates') this.summaryTextForDup();
			else if (t === 'corrupted_files') this.summaryTextForCorrupted();
			else this.summaryTextForFlat(t);
		},

		/* ------------------------------------------- magnifier search menu */
		// The dropdown behind the search box's magnifier, mirroring File
		// Station's advanced search: Keyword, Location, File Type (+custom
		// extension), Date (field + range) and Size (MB). Differences from
		// File Station, all data-honest: no content search (the daemon has no
		// content index), no Owner/Group (the scan does not record owners),
		// and Captured Date replaces Last Accessed (we have EXIF capture
		// times; atime is never collected). All filtering is server-side —
		// client-side filters would silently act on the loaded page only.
		getSearchMenu: function () {
			if (this.searchMenu) return this.searchMenu;
			var me = this;
			var f = this.buildSearchFields();
			/* A To date before the From date can only ever return nothing, so
			   the pickers constrain each other — File Station does exactly this
			   on its own two fields:
			     from select -> to.setMinValue(date)
			     to   select -> from.setMaxValue(date)
			   which greys the impossible days out in the other picker. Clearing
			   a date restores that side's original bound (FS has no Clear on
			   these, so this part is ours). */
			// DSM's own wide defaults, kept as the fallback for a date field no
			// file carries (nothing to derive a range from)
			this._dateBounds = { toMin: f.to.minValue, fromMax: f.from.maxValue };
			f.from.on('select', function () { me.refreshDateBounds(); });
			f.to.on('select', function () { me.refreshDateBounds(); });
			f.dateBy.on('select', function () { me.refreshDateBounds(); });
			var form = this.buildSearchForm(f);
			this.searchMenu = new UxMenu({
				// df-search-menu scopes the footer buttons' pill shape — see the
				// rule in the injected stylesheet for why a menu needs it and a
				// window does not.
				cls: 'df-app df-search-menu',
				items: [form],
				listeners: {
					show: function () { me.syncSearchMenu(); },
					/* Naming this menu as the pickers' parentMenu (above) keeps
					   the menu open while a picker is up — but it also puts the
					   picker inside this menu's chain, so MenuMgr stops treating
					   clicks on the other fields as "outside" and the picker
					   never auto-closes: both popups end up open at once. Close
					   it here instead, on any mousedown in the menu that is not
					   inside the picker itself or on the owning field's own
					   trigger. */
					afterrender: function (mnu) {
						mnu.getEl().on('mousedown', function (e) {
							me.closeDatePickers(e.getTarget());
						});
					}
				}
			});
			return this.searchMenu;
		},

		// searchCombo is one dropdown of the search menu, in File Station's
		// configuration: not editable, every option shown on click, local data
		// from (value, text) pairs.
		searchCombo: function (pairs, value, extra) {
				var c = new UxCombo(Ext.apply({
					editable: false, triggerAction: 'all', mode: 'local',
					store: new Ext.data.ArrayStore({ fields: ['v', 't'], data: pairs }), valueField: 'v', displayField: 't',
					// Ext's default list template renders the display field as
					// raw HTML. The Location list is fed from folder PATHS, which
					// are user-created data (a stored-XSS hazard: a folder named
					// with markup would run in the administrator's session the
					// moment the list opened), so every option is escaped here.
					tpl: '<tpl for="."><div class="x-combo-list-item">{t:htmlEncode}</div></tpl>',
					value: value, anchor: '100%'
				}, extra));
				/* A combo's dropdown renders to document.body by default, which
				   puts it OUTSIDE the menu element — Ext's MenuMgr then reads
				   the click on a list item as an outside click and hides the
				   whole menu, losing every option mid-edit. getListParent is
				   Ext's designed hook for exactly this: keep the list inside
				   the menu so the click counts as inside. */
				c.getListParent = function () {
					var m = this.el ? this.el.up('.x-menu') : null;
					return m ? m.dom : document.body;
				};
				return c;
		},

			/* Same class of problem for date pickers, different mechanism: the
			   field shows its picker with show(el, pos) — no third argument —
			   so the picker's parentMenu is undefined, MenuMgr treats our menu
			   as unrelated, and hides it. The picker has to be pre-built so
			   show() can be bound before the FIRST click; letting the field
			   build it lazily would leave that first open unbound.
			   Construction mirrors what SYNO.ux.DateField.onTriggerClick does
			   itself — ownerField + pickerCfg + the syno-ux-datefield-menu
			   class + the onDateMenuHide wiring — so this stays DSM's own
			   picker (a stock Ext.menu.DateMenu here makes the calendar stop
			   looking like File Station's). */
		bindDatePicker: function (field, end) {
			var me = this;
				if (!UxDateMenu) return field;
				var dm = new UxDateMenu(Ext.apply({
					cls: 'syno-ux-datefield-menu',
					ownerField: field,
					pickerCfg: field.getPickerCfg ? field.getPickerCfg() : {}
				}, field.menuCfg));
				me._dateMenus = (me._dateMenus || []).concat([dm]); // destroyed with the window
				if (field.onDateMenuHide) field.mon(dm, 'hide', field.onDateMenuHide, field);
				/* THE reason to pre-build carefully: DateField wires its select
				   relay inside the same lazy branch that CREATES the menu, so
				   assigning field.menu here skips it forever and a day click
				   never reaches the field — the picker highlights the day, the
				   value is never set and the menu never closes. onSelect is
				   what does setValue + fireEvent('select') + menu.hide(), so
				   wiring it
				   also restores the close-on-day-click that DSM's own menu-level
				   handler cannot do here (it calls hide(true), which the deep-
				   hide guard below deliberately refuses). */
				dm.on('select', field.onSelect, field);
				field.menu = dm;
				var stockShow = dm.show;
				dm.show = function (el, pos) {
					return stockShow.call(this, el, pos, me.searchMenu);
				};
				/* The year and month controls in DSM's picker are dropdown
				   BUTTONS whose lists are themselves menus, parented to this
				   picker. Ext.menu.Menu.onHide cascades a deep hide up the
				   parentMenu chain, so choosing a year would deep-hide the
				   picker — and, because the picker names the search menu as
				   ITS parent, tear the whole search menu down too. File
				   Station's picker stays open on a year change (verified in
				   DSM), and it only escapes the cascade because its picker has
				   no parentMenu at all. Ours needs one, so refuse
				   the deep hide instead: a plain hide() — what a day click,
				   closeDatePickers and MenuMgr.hideAll all use (hideAll passes
				   no argument; measured) — still closes it normally. */
				var stockHide = dm.hide;
				dm.hide = function (deep) {
					if (deep === true) return this;
					return stockHide.apply(this, arguments);
				};
				/* With no date chosen the field seeds its picker with TODAY, so
				   it opens on a month that may hold nothing. Open on the end of
				   the data instead: From on the oldest file's month, To on the
				   newest. update() moves only the displayed month — setValue()
				   would silently fill the field with a date the user never
				   picked (measured in DSM), and there is no select to undo
				   because setValue does not fire one. */
				var stockTrigger = field.onTriggerClick;
				field.onTriggerClick = function () {
					var r = stockTrigger.apply(this, arguments);
					var d = me.pickerOpenDate(end, !!this.getValue());
					if (d && this.menu && this.menu.picker && this.menu.picker.update) {
						this.menu.picker.update(d);
					}
					return r;
				};
				/* …but the trigger wrapper alone only covers the FIRST open:
				   every later open would show today, and since today is past
				   maxValue the calendar would arrive with all 42 day cells
				   disabled — nothing selectable, no hint where the data lives.
				   Traced in DSM: ~140ms after a re-open, DSM's own deferred
				   `updateDateTime` calls `setValue('')`, which calls
				   `update('')`, and an empty date makes the picker fall back to
				   today — landing after our wrapper has already run.
				   Racing it with a later update() would be guesswork, so instead
				   make the fallback itself correct: an update with no date shows
				   the data's month. This is timing-independent and cannot commit
				   a value, because update() only moves the displayed month —
				   picker.value stays empty (verified across repeated open/close
				   cycles: month right every time, field and picker.value both
				   still empty, and a real day click still commits and closes). */
				if (dm.picker && dm.picker.update) {
					var stockPickerUpdate = dm.picker.update;
					dm.picker.update = function (d, forceRefresh) {
						if (!d) {
							var openOn = me.pickerOpenDate(end, !!field.getValue());
							if (openOn) d = openOn;
						}
						return stockPickerUpdate.call(this, d, forceRefresh);
					};
				}
				/* Clear empties the field but leaves the calendar sitting on
				   today, which may hold nothing — send it back to where the
				   picker opened (the oldest/newest file's month). The button
				   lives inside the picker, so it can only be wrapped once the
				   menu has rendered; do it on the first show. update() is safe
				   here because the picker is already rendered and settled, and
				   it never writes a value into the field. */
				dm.on('show', function () {
					if (dm.__clearHooked || !dm.getEl || !dm.getEl()) return;
					var cel = dm.getEl().dom.querySelector('.clear-btn');
					var btn = cel ? Ext.getCmp(cel.id) : null;
					if (!btn) return;
					dm.__clearHooked = true;
					var stockHandler = btn.handler;
					btn.handler = function () {
						var r2 = stockHandler ? stockHandler.apply(this, arguments) : undefined;
						/* Clear fires no select, so nothing else recomputes the
						   pair's bounds: the OTHER field would go on being
						   limited by the date just removed, and From would
						   report "must be <date> or earlier" from a To that had
						   already been cleared. */
						me.refreshDateBounds();
						var back = me.pickerOpenDate(end, false);
						if (back && dm.picker && dm.picker.update) dm.picker.update(back);
						return r2;
					};
				});
				return field;
		},

		// buildSearchFields creates the search form's controls, keyed the way
		// applySearchMenu and syncSearchMenu read them (this.soFields).
		buildSearchFields: function () {
			var me = this;
			// paired fields sit side by side in a composite row, so they take
			// flex rather than anchor (File Station: flex 1 each, 8px gutter)
			var LEFT = { flex: 1, margins: '0 8 0 0' }, RIGHT = { flex: 1 };
			var f = this.soFields = {
				kw: new UxText({ fieldLabel: 'Keyword', anchor: '100%' }),
				loc: this.searchCombo([['', 'All folders in scope']], '', { anchor: '100%' }),
				type: this.searchCombo([
					['', 'Any'], ['dir', 'Any Folder'], ['file', 'Any File'],
					['docs', 'Documents'], ['video', 'Videos'], ['pic', 'Pictures'],
					['audio', 'Audio'], ['web', 'Web pages'], ['exe', 'Executable files'],
					['disk', 'Disk Image files'], ['zip', 'Zipped files'], ['ext', 'Extension']
				], '', Ext.apply({ listeners: { select: function (c) {
					me.soFields.ext.setDisabled(c.getValue() !== 'ext');
				} } }, LEFT)),
				ext: new UxText(Ext.apply({ emptyText: 'File extension', disabled: true }, RIGHT)),
				dateBy: this.searchCombo([
					['modified', 'Modified Date'], ['created', 'Created Date'],
					['captured', 'Captured Date']
				], 'modified', { anchor: '100%' }),
				// editable:false + format:'Y-m-d' + a From/To emptyText is
				// exactly how File Station configures these two fields
				from: this.bindDatePicker(new UxDate(Ext.apply({ format: 'Y-m-d',
					editable: false, emptyText: 'From' }, LEFT)), 'lo'),
				to: this.bindDatePicker(new UxDate(Ext.apply({ format: 'Y-m-d',
					editable: false, emptyText: 'To' }, RIGHT)), 'hi'),
				sizeOp: this.searchCombo([
					['', 'Any'], ['eq', 'equals'], ['gt', 'is greater than'], ['lt', 'is less than']
				], '', Ext.apply({ listeners: { select: function (c) {
					me.soFields.size.setDisabled(!c.getValue());
				} } }, LEFT)),
				size: new UxNumber(Ext.apply({ emptyText: 'Size (MB)',
					allowNegative: false, allowDecimals: false, disabled: true }, RIGHT))
			};
			return f;
		},

		// searchRow pairs two controls on one composite row, as File Station's
		// search form does.
		searchRow: function (label, left, right) {
				return new UxComposite(Ext.apply(
					label ? { fieldLabel: label } : { hideLabel: true },
					{ anchor: '100%', items: [left, right] }
				));
		},

		// buildSearchForm lays the controls out in File Station's row grouping
		// and adds the Reset and Search buttons.
		buildSearchForm: function (f) {
			var me = this;
			return new Ext.Panel({
				border: false, layout: 'form', labelAlign: 'top', width: 340,
				bodyStyle: 'padding:12px 14px 4px;',
				// File Station's own row grouping: the paired controls share a
				// composite row (File Type + extension, From + To, size
				// comparison + value); Keyword, Location and the date field
				// stand alone
				items: [
					f.kw,
					Ext.apply(f.loc, { fieldLabel: 'Location' }),
					this.searchRow('File Type', f.type, f.ext),
					Ext.apply(f.dateBy, { fieldLabel: 'Date' }),
					this.searchRow(null, f.from, f.to),
					this.searchRow('Size (MB)', f.sizeOp, f.size)
				],
				buttonAlign: 'right',
				buttons: [
					new UxButton({ text: 'Reset', handler: function () {
						me.resetSearchOpts();
						me.state.query = '';
						me.searchField.setValue('');
						me._soSynced = false;   // re-read the cleared state on next open
						me.syncSearchMenu();    // and clear the fields now
						me.applySearch();
					} }),
					new UxButton({ btnStyle: 'blue', text: 'Search', handler: function () {
						me.applySearchMenu();
					} })
				]
			});
		},

		// Hide any open date picker unless the click landed inside it (its own
		// month/year dropdowns render within the picker, so those are safe) or
		// on the trigger of the field that owns it.
		closeDatePickers: function (target) {
			var f = this.soFields;
			if (!f) return;
			Ext.each(['from', 'to'], function (k) {
				var pm = f[k] && f[k].menu;
				if (!(pm && pm.isVisible && pm.isVisible())) return;
				var pel = pm.getEl && pm.getEl();
				if (pel && target && pel.dom.contains(target)) return;
				var wrap = f[k].el ? f[k].el.up('.x-form-field-wrap') : null;
				if (wrap && target && wrap.dom.contains(target)) return;
				pm.hide();
			});
		},

		/* Menu show. The Location choices are always rebuilt (the scan scope can
		   change between opens), but the FIELD VALUES are only re-read from
		   state after an apply/reset — otherwise dismissing the menu without
		   pressing Search would silently discard everything typed into it.
		   Reset, Search and the X all set _soSynced=false to opt back in. */
		syncSearchMenu: function () {
			var f = this.soFields, so = this.state.searchOpts;
			var pairs = [['', 'All folders in scope']];
			Ext.each(this.state.dirs, function (d) { pairs.push([d, d]); });
			f.loc.store.loadData(pairs);
			if (!so.loc || !this.state.dirs || this.state.dirs.indexOf(so.loc) < 0) so.loc = '';
			if (!this._soSynced) { // otherwise keep the user's in-progress edits
				this._soSynced = true;
				f.kw.setValue(this.state.query || '');
				f.loc.setValue(so.loc);
				f.type.setValue(so.ftype);
				f.ext.setValue(so.extq);
				f.ext.setDisabled(so.ftype !== 'ext');
				f.dateBy.setValue(so.dateBy || 'modified');
				f.from.setValue(so.dateFrom || '');
				f.to.setValue(so.dateTo || '');
				f.sizeOp.setValue(so.sizeOp);
				f.size.setValue(so.sizeMB == null ? '' : so.sizeMB);
				f.size.setDisabled(!so.sizeOp);
			}
			// Always: a rescan can move the data span, and the picker limits
			// also depend on the values just restored above.
			this.refreshDateBounds();
		},

		applySearchMenu: function () {
			var f = this.soFields;
			function day(fld) {
				var v = fld.getValue();
				return v ? Ext.util.Format.date(v, 'Y-m-d') : '';
			}
			this.state.searchOpts = {
				loc: f.loc.getValue() || '',
				ftype: f.type.getValue() || '',
				extq: String(f.ext.getValue() || '').replace(/^[\s.]+|\s+$/g, ''),
				dateBy: f.dateBy.getValue() || 'modified',
				dateFrom: day(f.from),
				dateTo: day(f.to),
				sizeOp: f.sizeOp.getValue() || '',
				sizeMB: f.size.getValue() || 0
			};
			this.state.query = String(f.kw.getValue() || '').replace(/^\s+|\s+$/g, '');
			this._soSynced = false;   // applied: next open reflects state again
			// setValue alone — it fires no filter()/keyup, so no double reload
			this.searchField.setValue(this.state.query);
			if (this.searchMenu) this.searchMenu.hide();
			this.markSearchTrigger();
			this.applySearch();
		},

		/* The selectable range of the two pickers, from two independent limits:

		   1. THE DATA. A From before the oldest file, or a To after the newest,
		      can only ever return nothing, so the daemon reports the span of
		      each date field over the whole scan (not the current filter — the
		      pickers should always reach any file the scan found). Note this is
		      the newest FILE, never "today": a file can legitimately carry a
		      future timestamp, and clamping to today would hide it.
		   2. EACH OTHER. File Station narrows the counterpart as soon as one
		      end is chosen, so a To before the From is unpickable.

		   Both are recomputed together here — they overlap, and applying them
		   separately would let one clobber the other. Called on either select,
		   on a Date-field switch (each field has its own span) and on menu
		   sync. */
		refreshDateBounds: function () {
			var f = this.soFields;
			if (!f || !this._dateBounds) return;
			var span = this.dateSpanForField();
			var lo = span.lo || this._dateBounds.toMin;
			var hi = span.hi || this._dateBounds.fromMax;
			var fromVal = f.from.getValue(), toVal = f.to.getValue();
			f.from.setMinValue(lo);
			f.from.setMaxValue(toVal || hi);
			f.to.setMinValue(fromVal || lo);
			f.to.setMaxValue(hi);
			// A bound change can make a value that was illegal legal again (or
			// the reverse), and Ext only re-tests on its own edit events — so
			// re-validate here, or a field keeps showing an error against a
			// bound that no longer exists.
			if (f.from.rendered) f.from.validate();
			if (f.to.rendered) f.to.validate();
		},

		// Which month a picker should OPEN on. Null means "leave it alone":
		// either the field already holds a date (it seeds the picker itself) or
		// the field carries no dates to aim at. end is 'lo' for From, 'hi' for
		// To — so an empty From opens on the oldest file's month and an empty
		// To on the newest, instead of on today, which may hold nothing.
		pickerOpenDate: function (end, hasValue) {
			if (hasValue) return null;
			var span = this.dateSpanForField();
			return (end === 'hi' ? span.hi : span.lo) || null;
		},

		// Whole-scan span of the date field currently selected in the menu, as
		// Dates at day granularity (the stored values carry a time of day, and
		// the picker selects whole days). Either end is null when no file
		// carries that field.
		dateSpanForField: function () {
			var res = this.state.results[this.state.tool] || {};
			var ranges = res.dateRange || {};
			var by = (this.soFields && this.soFields.dateBy.getValue()) ||
				(this.state.searchOpts || {}).dateBy || 'modified';
			var span = ranges[by] || {};
			function day(v) {
				if (!v) return null;
				return Date.parseDate(String(v).substring(0, 10), 'Y-m-d');
			}
			return { lo: day(span.min), hi: day(span.max) };
		},

		hasSearchOpts: function () {
			var so = this.state.searchOpts || {};
			return !!(so.loc || so.ftype || ((so.dateFrom || so.dateTo) && so.dateBy) || so.sizeOp);
		},

		resetSearchOpts: function () {
			this.state.searchOpts = { loc: '', ftype: '', extq: '', dateBy: 'modified',
				dateFrom: '', dateTo: '', sizeOp: '', sizeMB: null };
			this._soSynced = false;   // the X / Reset cleared state: re-read it
			this.markSearchTrigger();
		},

		// light the magnifier while menu options are active, the way File
		// Station marks its search box (SearchField's own hook; absent on the
		// plain-TextField fallback)
		markSearchTrigger: function () {
			if (this.searchField && this.searchField.setSearchTriggerActive) {
				try { this.searchField.setSearchTriggerActive(this.hasSearchOpts()); } catch (e) { /* cosmetic */ }
			}
		},

		// Search asks the daemon (name or location substring): the grid holds
		// one page, so a local filter would silently miss everything off it.
		// Results narrow to the matches, from page one.
		applySearch: function () {
			if (!this.state.scanned[this.state.tool]) return;
			this.loadPage(0);
		},

		/* Wraps the summary in the width-capped span (see .df-summary) and
		   carries the untruncated wording as a tooltip, so a clipped summary
		   still gives up its full text on hover. The qtip must be the PLAIN
		   text — Ext QuickTips renders the attribute as markup, and the summary
		   contains <b>. */
		setSummary: function (html) {
			var plain = String(html).replace(/<[^>]*>/g, '');
			this.summaryText.setText('<span class="df-summary" ext:qtip="' + esc(plain) + '">' + html + '</span>');
			this.syncSummaryWidth();
		},

		/* Gives the summary every pixel the toolbar actually has, and not one
		   more. The toolbar's fill sits between the match button and the
		   summary, so on a roomy window the slack is parked there while a
		   fixed cap clips the text anyway; and at minWidth with the longest
		   match label even 380px pushed the search box past the edge. So:
		   let the summary size itself, then take back exactly the overflow.
		   The search field is always the last toolbar cell. */
		syncSummaryWidth: function () {
			if (!this.topToolbar || !this.topToolbar.getEl()) return;
			var tb = this.topToolbar.getEl().dom;
			var el = tb.querySelector('.df-summary');
			if (!el) return;
			var cells = [], all = tb.querySelectorAll('.x-toolbar-cell'), i;
			for (i = 0; i < all.length; i++) {
				if (all[i].getBoundingClientRect().width > 0) cells.push(all[i]);
			}
			var last = cells[cells.length - 1];
			if (!last) return;
			el.style.maxWidth = 'none';
			// 6px keeps a hair of clearance at the toolbar's right padding
			var over = Math.ceil(last.getBoundingClientRect().right -
				(tb.getBoundingClientRect().right - 6));
			if (over > 0) {
				el.style.maxWidth =
					Math.max(140, Math.floor(el.getBoundingClientRect().width - over)) + 'px';
			}
		},

		// The summary always describes the WHOLE result set; the pager under
		// the grid says which slice of it is on screen.
		summaryTextForDup: function () {
			var res = this.state.results.duplicates || {};
			var total = res.total || {};
			var txt = '<b>' + fmtCount(total.files || 0) + ' duplicate files</b> in ' + fmtCount(total.groups || 0) +
				((total.groups || 0) === 1 ? ' group' : ' groups') + ' · ' + fmtBytes(total.reclaimable || 0) + ' reclaimable';
			if (res.truncated) txt += ' · capped';
			this.setSummary(txt + this.issuesLink(res));
		},
		// Leads with what the scan could actually convict. A bare file count
		// would read as "48 corrupted files" when most of those rows are the
		// intact copy, or a pair the evidence could not separate.
		summaryTextForCorrupted: function () {
			var res = this.state.results.corrupted_files || {};
			var total = res.total || {};
			var sets = total.groups || 0;
			var txt = '<b>' + fmtCount(total.files || 0) + ' files</b> in ' + fmtCount(sets) +
				(sets === 1 ? ' set' : ' sets') + ' · ' + fmtCount(total.corrupt || 0) + ' identified as damaged';
			if (res.truncated) txt += ' · capped';
			this.setSummary(txt + this.issuesLink(res));
		},
		summaryTextForFlat: function (t) {
			var res = this.state.results[t] || {};
			var total = res.total || {};
			var lbl = { empty_folders: 'empty folders', empty_files: 'empty files', temp_files: 'temporary files' }[t];
			var rest = t === 'temp_files' ? ' · ' + fmtBytes(total.bytes || 0) + ' total' : '';
			var txt = '<b>' + fmtCount(total.files || 0) + ' ' + lbl + '</b>' + rest;
			if (res.truncated) txt += ' · capped';
			this.setSummary(txt + this.issuesLink(res));
		},

		// The summary's pointer to the scan's issue list — every unreadable
		// location, not only the first one the toast has room to quote. The
		// count is the only thing rendered, so nothing here needs escaping.
		issuesLink: function (res) {
			var n = res && res.errors ? res.errors.length : 0;
			if (!n) return '';
			return ' · <a href="#" class="df-issues">\u26a0 ' + fmtCount(n) + (n === 1 ? ' issue' : ' issues') + '</a>';
		},

		// Every issue the last scan of a tool recorded, in a scrollable list:
		// the locations the package user could not read (each one is a folder
		// to grant access to), the daemon's closing "… and N more" line when
		// its own list ran out, and anything else the scan had to say. Every
		// line is escaped — they carry folder names.
		openIssuesDialog: function (tool) {
			var res = this.state.results[tool];
			if (!res || !res.errors || !res.errors.length) return null;
			var n = res.errors.length;
			var who = this.state.serviceUser ? 'the "' + esc(this.state.serviceUser) + '" user' : "the package's user";
			var rows = [];
			Ext.each(res.errors, function (e) { rows.push('<div class="df-issue">' + esc(String(e)) + '</div>'); });
			var win = new UxModal({
				owner: this,
				title: 'Scan Issues — ' + (TOOL_NAME[tool] || tool),
				width: 640, height: 420, modal: false,
				cls: 'df-app',
				layout: 'vbox', layoutConfig: { align: 'stretch' },
				items: [
					{ xtype: 'panel', border: false, height: 48, bodyStyle: 'padding:10px 12px 0;font-size:' + FSMALL + ';color:' + FONT2 + ';line-height:1.4;',
						html: fmtCount(n) + (n === 1 ? ' issue' : ' issues') + ' from the last scan. A location that could not be read needs ' + who +
							' granted access under Control Panel → Shared Folder → Edit → Permissions → System internal user, then a new scan.' },
					{ xtype: 'panel', border: false, flex: 1, autoScroll: true, bodyStyle: 'padding:6px 12px;', cls: 'df-mono', html: rows.join('') }
				],
				buttons: [new UxButton({ btnStyle: 'blue', text: 'Close', handler: function () { win.close(); } })]
			});
			win.show();
			return win;
		},

		// A failed state save, as /api/state and the move response report it:
		// the results on screen are real but will not survive a restart, and
		// the user has to hear that. Kept on state so the completion toast can
		// carry it; cleared when the daemon reports a later save succeeded.
		noteSaveError: function (st) {
			this.state.saveError = st && st.saveError ? String(st.saveError) : '';
		},

		// The flat tools sort server-side, so the store's local sortInfo stays
		// on the arrival order ('ord') and the header arrow is painted
		// manually (refreshes wipe it — re-applied via the view's refresh
		// listener).
		updateFlatSortIcon: function () {
			var t = this.state.tool, srt = this._flatSort[t];
			// Grouped tools never sort server-side, so painting an arrow here
			// would claim an ordering the rows do not have.
			if (isGrouped(t) || !srt || !this.grid.rendered) return;
			var view = this.grid.getView();
			var ci = this.grid.getColumnModel().findColumnIndex(srt.field);
			if (ci >= 0 && view.updateSortIcon) view.updateSortIcon(ci, srt.dir);
		},

		// A header click on a flat tool (intercepted in store.sort): the whole
		// result set reorders, so the view returns to page one.
		onFlatSort: function (field, dir) {
			var t = this.state.tool;
			if (!this.state.scanned[t]) return;
			var cur = this._flatSort[t];
			if (!dir) dir = (cur && cur.field === field && cur.dir === 'ASC') ? 'DESC' : 'ASC';
			dir = String(dir).toUpperCase() === 'DESC' ? 'DESC' : 'ASC';
			this._flatSort[t] = { field: field, dir: dir };
			this.loadPage(0);
		},


		updateBadges: function () {
			var me = this;
			this.toolsStore.each(function (rec) {
				var id = rec.get('id'), badge = rec.get('badge') || '';
				if (!me.state.scanned[id]) {
					badge = '';
				} else if (me.state.results[id]) {
					// grand totals: the whole scan under the current match
					// refinement, ignoring the search box
					badge = fmtNum((me.state.results[id].grand || {}).files || 0);
				}
				rec.set('badge', badge);
				rec.commit();
			});
			this.toolsView.refresh();
		},

		updateSelectionUI: function () {
			// bulk selects fire selectionchange per row; the wrapper runs this
			// once at the end instead
			if (this._bulkSelecting) return;
			var recs = this.sm.getSelections(), bytes = 0;
			Ext.each(recs, function (r) { bytes += r.get('size') || 0; });
			if (isReadOnly(this.state.tool)) {
				// Says what to do next instead of offering an action the tool
				// does not have. Export stays available beside it.
				this.selText.setText('Review each set, then act on the files in File Station');
			} else if (recs.length) {
				this.selText.setText('<b>' + recs.length + (recs.length === 1 ? ' file' : ' files') + ' selected</b> · ' + fmtBytes(bytes) + ' reclaimable');
			} else {
				this.selText.setText('Select files to act on them');
			}
			this.moveBtn.setDisabled(!recs.length);
			var menu = this.selectMenuBtn.menu;
			if (menu && menu.items) {
				var isDup = this.state.tool === 'duplicates';
				menu.items.each(function (it) {
					if (it.itemId === 'selNewest' || it.itemId === 'selOldest') it.setVisible(isDup);
				});
			}
			if (this.bottomToolbar.rendered) this.bottomToolbar.doLayout();
			this.updateCheckerStates();
		},

		/* ------------------------------------------------------ selection */
		// True when selecting rec still leaves another file of its duplicate
		// group unselected (protected masters count — they can never move).
		// The daemon never splits a group across pages, so the page on screen
		// always holds the whole group this has to reason about.
		groupHasUnselectedPeer: function (rec) {
			if (this.state.tool !== 'duplicates') return true;
			var ids = (this._grpIds || {})[rec.get('gidx')] || [];
			var coll = this.store.data;
			for (var i = 0; i < ids.length; i++) {
				var r = coll.key(ids[i]);
				if (r && r !== rec && !this.sm.isSelected(r)) return true;
			}
			return false;
		},

		// Disable the checkbox of each group's last unselected file — the
		// keep-one rule means it can't be selected. Mirrors
		// groupHasUnselectedPeer, over the same complete groups.
		updateCheckerStates: function () {
			if (!this.grid.rendered) return;
			var view = this.grid.getView();
			if (!view.mainBody) return;
			var me = this, isDup = this.state.tool === 'duplicates', unsel = {};
			if (isDup) {
				this.store.data.each(function (r) {
					if (!me.sm.isSelected(r)) {
						var g = r.get('gidx');
						unsel[g] = (unsel[g] || 0) + 1;
					}
				});
			}
			// GroupingView.getRows() rebuilds the row array from every group's
			// childNodes on each call, and getRow(i) calls it — so asking per row
			// made this pass quadratic in the page size. Ask once.
			var rowEls = view.getRows ? view.getRows() : null;
			this.store.each(function (r, i) {
				var row = rowEls ? rowEls[i] : view.getRow(i);
				if (!row) return;
				var capped = isDup && !r.get('prot') && !me.sm.isSelected(r) &&
					unsel[r.get('gidx')] === 1;
				Ext.fly(row)[capped ? 'addClass' : 'removeClass']('df-row-capped');
				/* Say WHY the box cannot be ticked, on the box itself — this is
				   the hover the user gets when they try to check it.
				   Only the capped case: a protected row's cell holds a padlock,
				   not a box, and that padlock carries its own qtip straight
				   from the renderer — a second one on the cell around it would
				   just fight with it. */
				var cell = row.querySelector ? row.querySelector('.x-grid3-td-checker') : null;
				if (!cell) return;
				if (capped) cell.setAttribute('ext:qtip', 'At least one file in each group must stay unselected');
				else cell.removeAttribute('ext:qtip');
			});
		},

		// Keep-newest / keep-oldest bulk selection (duplicates only — the
		// menu items are hidden on the other tools).
		// Acts on the page on screen — like every other selection here, and
		// like File Station's own per-page selection.
		autoSelect: function (mode) {
			var wanted = {}, groups = {}, order = [], k;
			if (this.state.tool !== 'duplicates') return;
			this.store.each(function (r) {
				var g = r.get('gidx');
				if (!groups[g]) { groups[g] = []; order.push(g); }
				groups[g].push(r);
			});
			for (var i = 0; i < order.length; i++) {
				k = order[i];
				var recs = groups[k], movable = [], prot = 0;
				Ext.each(recs, function (r) {
					if (r.get('prot')) prot++;
					else movable.push(r);
				});
				if (prot > 0) {
					// a protected master survives, every movable copy may go
					Ext.each(movable, function (r) { wanted[r.id] = true; });
				} else {
					var keep = recs[0];
					Ext.each(recs, function (r) {
						if (mode === 'newest' ? r.get('date') > keep.get('date') : r.get('date') < keep.get('date')) keep = r;
					});
					Ext.each(recs, function (r) { if (r.id !== keep.id) wanted[r.id] = true; });
				}
			}
			var sel = [];
			this.store.each(function (r) { if (wanted[r.id]) sel.push(r); });
			this.bulkSelect(sel);
			if (this.store.getTotalCount() > this.pageUnitCount()) {
				this.notify('Applied to the ' + fmtNum(order.length) +
					' groups on this page — other pages are unchanged');
			}
		},

		// clear + select many records with a single UI refresh at the end
		// (the per-row beforerowselect guards still run for every record)
		bulkSelect: function (recs) {
			this._bulkSelecting = true;
			this.sm.clearSelections();
			this.sm.selectRecords(recs);
			this._bulkSelecting = false;
			this.updateSelectionUI();
		},

		openDirSelectDialog: function () {
			var me = this, t = this.state.tool;
			if (!this.state.scanned[t]) {
				this.notify('Run a scan first');
				return;
			}
			// the rows on the current page — selection never reaches past it
			var files = [];
			this.store.each(function (r) { files.push(r.data); });
			var nodes = {};
			Ext.each(files, function (f) {
				var parts = f.path.split('/'), cur = '', parent, i;
				for (i = 0; i < parts.length; i++) {
					if (!parts[i]) continue;
					parent = cur;
					cur = cur + '/' + parts[i];
					if (!nodes[cur]) nodes[cur] = { name: parts[i], path: cur, parent: parent, children: {}, fileIds: [] };
					if (parent) nodes[parent].children[cur] = true;
				}
				if (!f.prot) nodes[cur].fileIds.push(f.id);
			});
			var countUnder = function (p) {
				var n = nodes[p], c = n.fileIds.length, k;
				for (k in n.children) if (n.children.hasOwnProperty(k)) c += countUnder(k);
				return c;
			};
			var collectUnder = function (p, out) {
				var n = nodes[p], k;
				Ext.each(n.fileIds, function (id) { out[id] = true; });
				for (k in n.children) if (n.children.hasOwnProperty(k)) collectUnder(k, out);
			};
			var mkNode = function (p) {
				var n = nodes[p], kids = [], k;
				for (k in n.children) if (n.children.hasOwnProperty(k)) kids.push(mkNode(k));
				kids.sort(function (a, b) { return a.text < b.text ? -1 : 1; });
				var c = countUnder(p);
				// TreeNode text renders as raw HTML — folder names are
				// user-created data and must be escaped (stored-XSS hazard)
				return {
					text: esc(n.name) + '  (' + c + (c === 1 ? ' file' : ' files') + ')',
					checked: false, expanded: true, dfPath: p,
					leaf: !kids.length, children: kids
				};
			};
			var roots = [], k;
			for (k in nodes) {
				if (nodes.hasOwnProperty(k) && nodes[k].parent === '') roots.push(mkNode(k));
			}
			// SYNO.ux.TreePanel is a thin subclass of Ext's that adds DSM's
			// FleXcroll scrollbar and ARIA wiring; autoScroll is left off so
			// autoFlexcroll owns the scrolling (it falls back to Ext's tree,
			// and to autoScroll, where SYNO.ux.TreePanel is absent).
			var tree = new UxTree({
				border: false, rootVisible: false, useArrows: true,
				autoFlexcroll: !!SYNO.ux.TreePanel,
				autoScroll: !SYNO.ux.TreePanel,
				root: new Ext.tree.AsyncTreeNode({ text: '.', expanded: true, children: roots }),
				loader: new Ext.tree.TreeLoader() // static children
			});
			// the tree covers this page only — say so when there are more
			var partial = this.store.getTotalCount() > this.pageUnitCount();
			if (partial) tree.flex = 1;
			var win = new UxModal({
				owner: me,
				title: 'Select Files in Folder',
				width: 460, height: 430, modal: false,
				layout: partial ? 'vbox' : 'fit',
				layoutConfig: partial ? { align: 'stretch' } : undefined,
				cls: 'df-app',
				items: partial ? [{
					xtype: 'panel', border: false, height: 38,
					bodyStyle: 'padding:8px 10px;font-size:' + FSMALL + ';color:' + FONT2 + ';',
					html: 'Only the results on this page are listed — other pages are unchanged.'
				}, tree] : [tree],
				buttons: [
					new UxButton({ btnStyle: 'blue', text: 'Select Files', handler: function () {
						var wanted = {};
						tree.getRootNode().cascade(function (node) {
							if (node.attributes.checked && node.attributes.dfPath) collectUnder(node.attributes.dfPath, wanted);
						});
						var recs = [];
						me.store.each(function (r) { if (wanted[r.id]) recs.push(r); });
						me.bulkSelect(recs);
						win.close();
					} }),
					new UxButton({ text: 'Cancel', handler: function () { win.close(); } })
				]
			});
			win.show();
		},

		/* ------------------------------------------------------- scoping */
		openScopeDialog: function () {
			var me = this;
			var mkList = function (data) {
				return new Ext.list.ListView({
					store: new Ext.data.ArrayStore({ fields: ['path'], data: data }),
					// {path:htmlEncode}: folder names are user-created data and
					// XTemplate does not escape by default (stored-XSS hazard)
					columns: [{ header: 'Folder', dataIndex: 'path', tpl: '<span class="df-mono">{path:htmlEncode}</span>' }],
					hideHeaders: true,
					singleSelect: true,
					// DSM's themed hover/selection classes (same trick as the
					// sidebar rail) — the default x-list-over/-selected classes
					// are not styled by the DSM skin, leaving the clicked row
					// with no visible highlight.
					overClass: 'x-grid3-row-over',
					selectedClass: 'x-grid3-row-selected',
					flex: 1
				});
			};
			var toData = function (arr) {
				var out = [];
				Ext.each(arr, function (p) { out.push([p]); });
				return out;
			};
			var scopeList = mkList(toData(this.state.dirs));
			var refList = mkList(toData(this.state.refDirs));
			var sync = function () {
				scopeList.getStore().loadData(toData(me.state.dirs));
				refList.getStore().loadData(toData(me.state.refDirs));
				me.savePrefs();
			};
			var win = new UxModal({
				owner: me,
				title: 'Scan Scope',
				cls: 'df-app',
				width: 520, height: 552, modal: false,
				layout: 'vbox', layoutConfig: { align: 'stretch' },
				bodyStyle: 'padding:10px;',
				items: [
					{ xtype: 'panel', border: false, height: 22, html: '<b>Folders to scan</b>' },
					scopeList,
					{ xtype: 'panel', border: false, height: 46, bodyStyle: 'padding-top:4px;', items: new UxToolbar({ items: [
						{ text: 'Add Folder…', handler: function () {
							pickFolder({
								owner: win,
								onError: function (msg) { me.notify(msg); },
								title: 'Add Folder to Scope',
								onPick: function (p) {
									if (p && me.state.dirs.indexOf(p) < 0) me.state.dirs.push(p);
									sync();
								}
							});
						} },
						{ text: 'Remove', handler: function () {
							var r = scopeList.getSelectedRecords()[0];
							if (r) { me.state.dirs.remove(r.get('path')); sync(); }
						} }
					] }) },
					{ xtype: 'panel', border: false, height: 52, bodyStyle: 'padding-top:10px;line-height:1.4;', html: '<b>Read-only reference folders</b> <span style="color:' + FONT3 + ';">— files inside are protected masters: they are never selected or moved.</span>' },
					refList,
					{ xtype: 'panel', border: false, height: 46, bodyStyle: 'padding-top:4px;', items: new UxToolbar({ items: [
						{ text: 'Add Reference Folder…', handler: function () {
							pickFolder({
								owner: win,
								onError: function (msg) { me.notify(msg); },
								title: 'Add Reference Folder',
								onPick: function (p) {
									if (p && me.state.refDirs.indexOf(p) < 0) me.state.refDirs.push(p);
									me.sm.clearSelections();
									sync();
									if (p) me.notify(p + ' marked read-only');
								}
							});
						} },
						{ text: 'Remove', handler: function () {
							var r = refList.getSelectedRecords()[0];
							if (r) { me.state.refDirs.remove(r.get('path')); me.sm.clearSelections(); sync(); }
						} }
					] }) }
				],
				buttons: [new UxButton({ btnStyle: 'blue', text: 'Close', handler: function () {
					win.close();
					// The padlocks and the reclaimable totals follow the folders
					// the stored results were SCANNED with (the daemon's list),
					// not this dialog's: unlocking rows here would only have the
					// move refuse them as read-only. Say when the lists differ.
					var norm = function (a) { return (a || []).slice().sort().join('\n'); };
					if (norm(me.state.refDirs) !== norm(me.state.scanRefDirs || [])) {
						me.notify('Reference folder changes take effect at the next scan.');
					}
				} })]
			});
			win.show();
		},

		/* ------------------------------------------------------ scanning */
		startScan: function () {
			var me = this;
			if (!this.state.dirs.length) {
				this.notify('Add at least one folder to the scope first (Scope… button)');
				return;
			}
			// The choice dialog is already on screen: a second Scan click must
			// not slip past it into a fresh scan — that would clear the
			// daemon's notice and destroy the very resume the dialog is
			// offering.
			if (liveWin(this._resumeWin)) return;
			// An interrupted duplicates scan gets a real choice at the moment
			// the user initiates the next one — not an unsolicited dialog at
			// app open. Resume continues the dead run: the files it already
			// read in full are not read again, which is what resuming MEANS;
			// Start Over re-reads everything. Only duplicates is offered the
			// choice — the other tools read no content, so re-running them IS
			// their resume. Any started scan clears the notice on the daemon,
			// so the offer cannot outlive what it describes.
			var inter = this._interrupted;
			// Any admitted scan clears the daemon's notice — and the marker,
			// and with it the dead run's generation for good. The toast at
			// startup promised the next Scan would offer to resume it, so a
			// Scan on some OTHER tool must ask before it makes that promise
			// false; "resume" is never something the app decides silently, and
			// neither is "discard".
			if (inter && inter.tool === 'duplicates' &&
					scanToolFor(this.state.tool) !== 'duplicates') {
				this.confirmDiscardResume();
				return;
			}
			if (inter && inter.tool === 'duplicates' &&
					scanToolFor(this.state.tool) === 'duplicates') {
				// Revalidate before promising anything: another session may
				// have started a scan since this stash was taken, which clears
				// the daemon's notice and makes the resume flag inert — the
				// dialog must not describe a resume that will not happen. A
				// /state ERROR keeps the offer: a transient hiccup must never
				// silently discard a resumable run, and the scan POST will
				// surface any real problem itself.
				api('/state', 'GET', null, function (err, st) {
					if (!err && st && (!st.interrupted || st.interrupted.tool !== 'duplicates')) {
						me._interrupted = null;
						me.doScan(false);
						return;
					}
					if (!err && st && st.interrupted) me._interrupted = st.interrupted;
					me.offerResume();
				});
				return;
			}
			this.doScan(false);
		},

		// Whether the interrupted run's recorded request matches what Scan
		// would send right now. The daemon enforces the same rule (a resume
		// is honored only for the identical request — scope, reference
		// folders, recursion, match criteria), so the dialog must not offer
		// a Resume the daemon would quietly turn into a full re-read. A
		// marker from an older build records no request and never matches —
		// the safe direction, same as the daemon's.
		resumeMatchesSettings: function () {
			var m = this._interrupted || {};
			// Cleaned like the daemon cleans the marker (filepath.Clean), then
			// deduplicated and sorted — otherwise a trailing slash in a stored
			// preference makes this say "changed" for a request the daemon
			// would resume.
			var norm = function (a) {
				a = (a || []).slice();
				for (var j = 0; j < a.length; j++) a[j] = cleanPath(a[j]);
				a.sort();
				var out = [];
				for (var i = 0; i < a.length; i++) {
					if (!i || a[i] !== out[out.length - 1]) out.push(a[i]);
				}
				return out.join('\n');
			};
			var mm = m.match || {};
			return m.recurse === true &&
				norm(m.dirs) === norm(this.state.dirs) &&
				norm(m.refDirs) === norm(this.state.refDirs) &&
				!!mm.name === !!this.state.match.name &&
				!!mm.modified === !!this.state.match.modified &&
				!!mm.created === !!this.state.match.created;
		},

		// Asked when a Scan on another tool would discard the interrupted
		// duplicates run. Shares _resumeWin so a second Scan click while it is
		// open returns instead of starting the scan behind the question.
		confirmDiscardResume: function () {
			var me = this;
			if (liveWin(this._resumeWin)) return;
			var win = new UxModal({
				owner: me,
				title: 'Discard the interrupted scan?',
				width: 430,
				autoHeight: true,
				modal: false,
				cls: 'df-app',
				bodyStyle: 'padding:14px;font-size:' + FSMALL + ';',
				items: [{ xtype: 'panel', border: false,
					html: 'The last duplicate files scan was interrupted by a restart and can still be resumed.<br/><br/>' +
						'Scanning ' + esc(TOOL_NAME[scanToolFor(this.state.tool)] || '') + ' now discards that progress: ' +
						'the next Duplicate Files scan will re-read everything.' }],
				buttons: [
					new UxButton({ btnStyle: 'blue', text: 'Scan Anyway', handler: function () {
						win.close();
						me.doScan(false);
					} }),
					new UxButton({ text: 'Cancel', handler: function () { win.close(); } })
				]
			});
			me._resumeWin = win;
			win.show();
		},

		// The Resume / Start Over choice for an interrupted duplicates scan.
		// Neither button clears _interrupted — only a scan the daemon ACCEPTS
		// does (doScan's success callback): a refusal like "a move is running"
		// must leave the offer standing for the retry, or the resumable run is
		// silently discarded by a click that moved nothing forward. When the
		// current settings no longer match the interrupted run's, Resume is
		// shown disabled with the reason: restoring the settings and clicking
		// Scan again re-enables it (the dialog consumes nothing).
		offerResume: function () {
			var me = this;
			if (liveWin(this._resumeWin)) return;
			var same = this.resumeMatchesSettings();
			var win = new UxModal({
				owner: me,
				title: 'Interrupted Scan',
				width: 430,
				autoHeight: true,
				modal: false,
				cls: 'df-app',
				bodyStyle: 'padding:14px;font-size:' + FSMALL + ';',
				items: [{ xtype: 'panel', border: false,
					// The started time says how stale a resume would be — a
					// run from weeks ago is worth knowing about before
					// choosing to continue its reads.
					html: 'The last duplicate files scan was interrupted by a restart' +
						(this._interrupted && this._interrupted.startedAt
							? ' (started ' + esc(String(this._interrupted.startedAt).replace('T', ' ').slice(0, 16)) + ')'
							: '') + '.<br/><br/>' +
						'<b>Resume</b> continues where it left off — files it already compared are not read again.<br/>' +
						'<b>Start Over</b> discards that progress and re-reads everything.' +
						(same ? '' : '<br/><br/>Resume is unavailable: the scan settings (scope, reference folders ' +
							'or match criteria) have changed since that run. Restore them to resume, ' +
							'or Start Over with the current settings.') }],
				buttons: [
					new UxButton({ btnStyle: 'blue', text: 'Resume', disabled: !same, handler: function () {
						win.close();
						me.doScan(true);
					} }),
					new UxButton({ text: 'Start Over', handler: function () {
						win.close();
						me.doScan(false);
					} }),
					new UxButton({ text: 'Cancel', handler: function () { win.close(); } })
				]
			});
			me._resumeWin = win;
			win.show();
		},

		doScan: function (resume) {
			var me = this;
			api('/scan', 'POST', {
				// Conflicting Files has no scan of its own — it is produced by the
				// duplicates pass, the only one that reads file contents — so
				// its Scan button runs that. The daemon refuses the read-only id
				// outright, so this mapping is not optional.
				tool: scanToolFor(this.state.tool),
				dirs: this.state.dirs,
				refDirs: this.state.refDirs,
				recurse: true, // subfolders are always scanned (no user option)
				// selected criteria join the pre-hash candidate key server-side,
				// so files unique in (size + criteria) are never hashed
				match: {
					name: !!this.state.match.name,
					modified: !!this.state.match.modified,
					created: !!this.state.match.created
				},
				// Honored by the daemon only while its interruption notice
				// stands and only for the same tool — everywhere else it is
				// inert, so this can never revive a completed scan's hashes.
				resume: !!resume
			}, function (err) {
				if (err) {
					me.notify(err);
					return;
				}
				me._interrupted = null;
				me.sm.clearSelections();
				me.attachProgress();
			});
		},

		attachProgress: function () {
			var me = this;
			if (this.progressWin) return;
			// Label sits above the bar (Ext draws in-bar text clipped at the
			// bar's height); the bar shows only the fill.
			this.progressLabel = new Ext.Panel({
				border: false,
				bodyStyle: 'padding:0 0 9px;font-size:' + FSMALL + ';color:' + FONT2 + ';',
				html: 'Initializing scan…'
			});
			this.progressBar = new Ext.ProgressBar({ height: 18, animate: false });
			// Not modal: a modal mask covers the whole DSM desktop and blocks
			// every other app. owner masks only this app's window, so the rest
			// of the desktop stays usable while scanning.
			this.progressWin = new UxModal({
				owner: this,
				title: 'Scanning ' + TOOL_NAME[scanToolFor(this.state.tool)] + '…',
				width: 420, autoHeight: true, modal: false, closable: false, resizable: false,
				cls: 'df-app',
				bodyStyle: 'padding:16px;',
				items: [this.progressLabel, this.progressBar],
				buttons: [new UxButton({ text: 'Stop', handler: function () {
					api('/cancel', 'POST', {}, function (err) {
						// the poll notices a successful stop; a failed one has
						// to be said, or the button looks dead
						if (err) me.notify('Could not stop the scan: ' + err);
					});
				} })]
			});
			this.progressWin.show();
			this._pollBusy = false;
			this._pollErrors = 0;
			this.pollTask = {
				run: this.pollStatus,
				scope: this,
				interval: 400
			};
			Ext.TaskMgr.start(this.pollTask);
		},

		stopPolling: function () {
			if (this.pollTask) {
				Ext.TaskMgr.stop(this.pollTask);
				this.pollTask = null;
			}
			// The move-progress poller is a plain interval, so closing the app
			// window mid-move would otherwise leave it firing against a
			// destroyed dialog for the life of the page.
			if (this._movePoll) {
				clearInterval(this._movePoll);
				this._movePoll = null;
			}
			if (this.progressWin) {
				this.progressWin.destroy();
				this.progressWin = null;
				this.progressBar = null;
				this.progressLabel = null;
			}
			// No unmask here on purpose: destroying an owned ModalWindow runs
			// DSM's onBeforeDestroy -> hideFromOwner, which de-registers it and
			// unmasks the owner through the reference-counted path. A raw
			// getEl().unmask() alongside that decrements nothing and can strand
			// the mask when another dialog is still open.
		},

		// Consecutive failed polls before the dialog is torn down: about half
		// a minute at the 400 ms cadence — long enough to ride out a package
		// restart, short enough that a daemon that is not coming back does
		// not leave the app masked behind an undismissable dialog for ever.
		pollGiveUp: 60,

		pollStatus: function () {
			var me = this;
			// One /state in flight at a time: each call is a full CGI spawn on
			// the NAS, and on a slow model a reply slower than the interval
			// would stack them without bound — and at scan end every outstanding
			// callback would run the completion refresh again.
			if (this._pollBusy) return;
			this._pollBusy = true;
			api('/state', 'GET', null, function (err, st) {
				me._pollBusy = false;
				if (err) {
					me._pollErrors = (me._pollErrors || 0) + 1;
					if (me._pollErrors >= me.pollGiveUp) {
						me.stopPolling();
						me.notify('Lost contact with the Duplicate Finder service — the scan may still be running on the NAS. Reopen the app to check.');
					}
					return; // transient until proven otherwise
				}
				me._pollErrors = 0;
				me.noteSaveError(st);
				// The daemon died mid-scan and came back: a poll then sees
				// running:false WITH the interruption notice (a healthy scan
				// always has the notice nil — any admission clears it). The
				// completion path below would announce the reloaded cached
				// results as a fresh finish and drop the notice on the floor;
				// stash it and say what actually happened instead, so the next
				// Scan click offers the resume the toast at startup promises.
				if (st.interrupted && st.interrupted.tool && TOOL_NAME[st.interrupted.tool]) {
					me._interrupted = st.interrupted;
					if (!st.running) {
						me.stopPolling();
						me.notify('The ' + TOOL_NAME[st.interrupted.tool].toLowerCase() +
							' scan was interrupted by a restart' +
							(st.interrupted.tool === 'duplicates'
								? ' — the next Scan will offer to resume it' : '') + '.');
						return;
					}
				}
				if (st.running) {
					if (me.progressBar) {
						me.progressBar.updateProgress((st.progress || 0) / 100);
						if (me.progressLabel && me.progressLabel.body) {
							// the label is daemon text, but it lands in innerHTML: escape it
							me.progressLabel.body.update(esc(st.label || '') + '  (' + Math.round(st.progress || 0) + '%)');
						}
					}
					return;
				}
				// A run that ENDED without completing — Stop, or a failure the
				// daemon reported — is no completion: replaying the previous
				// finished scan's announcements here (its unreadable-locations
				// toast, a jump back to page 1) would be exactly wrong.
				if (st.lastEnd && st.lastEnd.completed === false) {
					me.stopPolling();
					me.notify('Scan stopped.');
					return;
				}
				// The reference folders the new results were scanned with.
				me.state.scanRefDirs = st.refDirs || [];
				// lastTool is the scan that actually finished — the running
				// job is already cleared here, and the user may have switched
				// tools mid-scan, so the current selection can be wrong
				var tool = st.lastTool || st.tool || me.state.tool;
				me.stopPolling();
				// A duplicates scan also fills in Conflicting Files, and this is
				// the only place a finished scan refreshes anything: without the
				// second fetch that rail badge — and the view, if it is the one
				// on screen — would stay stale until the app was reopened.
				var refreshed = [tool], id;
				for (id in READ_ONLY) {
					if (READ_ONLY.hasOwnProperty(id) && scanToolFor(id) === tool) refreshed.push(id);
				}
				var pending = refreshed.length;
				var settle = function () {
					if (--pending > 0) return;
					// One toast, for whichever of the refreshed tools the user
					// is actually looking at — its wording and its counts differ
					// per tool, and notify() replaces rather than stacks.
					var onScreen = false;
					Ext.each(refreshed, function (r) { if (r === me.state.tool) onScreen = true; });
					me.announceScanIssues(onScreen ? me.state.tool : tool);
					me.refreshView();
					// show the finished scan's first page when it is on screen
					if (onScreen) me.loadPage(0);
				};
				Ext.each(refreshed, function (r) { me.fetchMeta(r, settle); });
			});
		},

		// Reads a tool's whole-set metadata (totals, errors, truncation)
		// without putting it on screen — one row is enough to carry it. The
		// rail badges describe every scanned tool, not just the visible one,
		// so they are filled in this way at startup.
		fetchMeta: function (tool, done) {
			var me = this;
			var body = Ext.apply({ tool: tool, offset: 0, limit: 1 }, this.fetchParams(tool));
			api('/results', 'POST', body, function (err, j) {
				if (!err && j && j.scanned) me.noteResultMeta(tool, j);
				if (done) done();
			}, { timeout: PAGE_TIMEOUT });
		},

		// One toast covering the scan-time issues worth announcing once:
		// unreadable locations and the stored-results cap. notify() replaces
		// any previous toast, so both must share a single message.
		announceScanIssues: function (tool) {
			var res = this.state.results[tool];
			if (!res) return;
			var parts = [];
			if (this.state.saveError) parts.push(this.state.saveError);
			if (res.errors && res.errors.length) {
				var first = String(res.errors[0]);
				if (first.length > 90) first = first.substring(0, 90) + '…';
				var who = this.state.serviceUser ? 'the "' + this.state.serviceUser + '" user' : "the package's user";
				parts.push(res.errors.length + (res.errors.length === 1 ? ' issue' : ' issues') +
					': ' + first + ' — a location that could not be read needs ' + who + ' granted access under Control Panel → Shared Folder → Edit → Permissions → System internal user' +
					(res.errors.length > 1 ? '; the summary bar links to the full list' : ''));
			}
			if (res.truncated) {
				var tr = res.truncated;
				if (tool === 'duplicates') {
					parts.push('Results were capped at the ' + fmtNum(tr.cap) +
						' duplicate files with the most reclaimable space — ' + fmtNum(tr.files) +
						' more duplicates were found. Narrow the scan scope to see the rest.');
				} else if (tool === 'corrupted_files') {
					parts.push('Results were capped at ' + fmtNum(tr.cap) +
						' files — ' + fmtNum(tr.files) + ' more were found in sets that disagree. ' +
						'Sets holding a file already identified as damaged are listed first. ' +
						'Narrow the scan scope to see the rest.');
				} else {
					parts.push('Results were capped at ' + fmtNum(tr.cap) + ' rows — ' + fmtNum(tr.files) +
						' more were found. Narrow the scan scope to see the rest.');
				}
			}
			if (parts.length) this.notify(parts.join('  '));
		},

		/* -------------------------------------------------- move / export */
		// The preserve option is answered BEFORE the folder picker, because it
		// changes what the picked folder MEANS: with it ticked the move creates
		// a new folder INSIDE the one that gets picked. Asked afterwards, the
		// picker's Select button would be ambiguous. The picker is the last
		// step, and its Select starts the move — so this dialog says so, and
		// there is no confirmation after it.
		openMoveDialog: function () {
			var me = this;
			var recs = this.sm.getSelections();
			if (!recs.length) return;
			// MEASURED ON DSM 7.4 — read this before simplifying anything below.
			//
			// Both this dialog and the folder chooser are owned
			// SYNO.SDS.ModalWindows, and DSM's afterShow masks the owner
			// REGARDLESS of `modal: false` (modal masks the whole desktop;
			// owner masks just this app). So for the entire options-dialog +
			// chooser flow the app window carries a full-window
			// .ext-el-mask.sds-window-mask: elementFromPoint inside the window
			// returns the mask, and the grid, the rail and the toolbar are all
			// click-blocked.
			//
			// Everything below is therefore DEFENCE IN DEPTH, not a fix for
			// something a user can currently do. It is kept because every one
			// of these guards is free, and because the masking is DSM's
			// behaviour rather than ours: a Chooser that ever sets `sinkable`,
			// or the Vue branch of afterShow, would drop the mask and hand the
			// app back to the user mid-flow with no warning to us.
			//
			// Re-selection guard: destroyed or hidden both mean the flow is
			// over, so this can never leave the Move button dead.
			if (liveWin(this._moveWin)) return;
			var n = recs.length;
			var noun = itemNoun(this.state.tool, n);
			// Captured, not read back at request time, so the batch carries the
			// tool it was selected under even if state.tool changes underneath.
			var tool = this.state.tool;
			// Fingerprint of the batch this dialog was opened for. The records
			// are captured, so a selection REPLACED between here and the pick
			// would move rows the user is no longer pointing at — and the pick
			// IS the move, there is no confirmation after it. Ids, not record
			// objects: reloading a page rebuilds records for the same rows
			// (JsonReader idProperty 'id', so ids are the daemon's and stable),
			// and re-selecting the same files must not count as a change.
			//
			// `=== true` rather than a bare truthiness test: wantIds is a plain
			// object, so a row id of 'constructor' or 'toString' would
			// otherwise read as present through Object.prototype. Unreachable
			// while the daemon issues "f<n>" ids, and free to not depend on.
			var wantIds = {}, wantCount = recs.length;
			Ext.each(recs, function (r) { wantIds[r.id] = true; });
			// Two mutually exclusive destination layouts, so say so with radios
			// rather than a checkbox: "preserve" is a choice between two
			// outcomes, not a modifier, and a tickbox would make the alternative
			// invisible until you read the prose.
			var mode = new UxRadioGroup({
				columns: 1,
				hideLabel: true,
				items: [
					{ boxLabel: 'Preserve original folder structure',
						name: 'df-move-mode', inputValue: 'preserve', checked: true },
					{ boxLabel: 'Move all files into same folder',
						name: 'df-move-mode', inputValue: 'flat' }
				]
			});
			// Whether to verify the move is the USER's call, made fresh each
			// time: keeping the moved files (above all on a remote destination)
			// argues for it; deleting them right away makes it pure cost — and
			// the app cannot predict those plans, so it asks, defaulting to the
			// safe side. Deliberately NO auto-detection of "same shared folder,
			// verification would only re-read the same blocks": a path can look
			// local and be a remote mount underneath, and guessing wrong in
			// that direction is exactly the unverified remote move the option
			// exists to prevent.
			var verifyBox = new UxCheckbox({
				boxLabel: 'Verify file contents after moving',
				hideLabel: true,
				checked: true
			});
			var win = new UxModal({
				owner: me,
				title: 'Move ' + n + ' ' + noun,
				width: 420,
				// autoHeight, like showBusy: without it the window takes a fixed
				// height, and a body of overflow:hidden then CLIPS whatever sits
				// last — a control rendered, ticked and functional with only six
				// pixels of it on screen.
				autoHeight: true,
				modal: false,
				cls: 'df-app',
				bodyStyle: 'padding:14px;',
				// The choice goes FIRST — it is what the dialog is asking — so it
				// can never be the thing clipped if this body ever overflows.
				items: [
					mode,
					{ xtype: 'panel', border: false, bodyStyle: 'padding-top:10px;', items: [verifyBox] },
					{ xtype: 'panel', border: false,
						bodyStyle: 'padding-top:10px;font-size:' + FSMALL + ';color:' + FONT2 + ';font-style:italic;',
						html: 'No files will be overwritten.' }
				],
				buttons: [
					new UxButton({ btnStyle: 'blue', text: 'Choose Folder…', handler: function () {
						// Read the choice BEFORE close() — close() destroys the
						// window and its child components. Anything that is not
						// an explicit "flat" means preserve: that is both the
						// default and the safer outcome, since a preserve batch
						// lands in its own new folder and can neither merge into
						// an earlier one nor rename a file.
						var picked = mode.getValue();
						var doPreserve = !picked || picked.inputValue !== 'flat';
						// Read like the radios — before close() destroys the
						// children. Anything that is not an explicit false
						// means verify: the checked default and the safe side.
						var doVerify = verifyBox.getValue() !== false;
						// And close before opening the chooser: while this
						// dialog is open it is the app window's topmost modal
						// child, so DSM would raise it back over the chooser
						// the moment the app window is activated.
						win.close();
						pickFolder({
							owner: me,
							title: 'Move ' + n + ' ' + noun + ' to…',
							onError: function (msg) { me.notify(msg); },
							onPick: function (dest) {
								// Require the selection to still be the exact
								// set this dialog was opened for. A finished
								// scan or a store reload can still empty or
								// rebuild it under the mask, and if DSM ever
								// stops masking the owner (see the note in
								// openMoveDialog) an outright swap becomes
								// possible too. Emptiness is just the case
								// where the count fell to zero.
								//
								// Set equality by count + membership is sound
								// because Ext 3.4's RowSelectionModel holds
								// selections in a MixedCollection keyed by
								// record id, so `now` cannot repeat an id.
								var now = me.sm.getSelections(), same = now.length === wantCount;
								if (same) {
									Ext.each(now, function (r) {
										if (wantIds[r.id] !== true) { same = false; return false; }
									});
								}
								if (!same) {
									me.notify('The selection changed while the folder was being chosen — nothing was moved.');
									return;
								}
								me.doMove(recs, dest, doPreserve, tool, doVerify);
							}
						});
					} }),
					new UxButton({ text: 'Cancel', handler: function () { win.close(); } })
				]
			});
			me._moveWin = win;
			win.show();
		},

		// tool names the result set the selection came from. It is captured by
		// openMoveDialog rather than read from state here, because the rail
		// stays clickable while the folder chooser is up — see the capture
		// there. It only ever labels the folder the daemon creates; the move's
		// vetting is unaffected by it.
		doMove: function (recs, dest, preserve, tool, verify) {
			var me = this;
			var paths = [];
			Ext.each(recs, function (r) { paths.push(r.get('path') + '/' + r.get('name')); });
			// Two folder choosers can be open at once (see openMoveDialog), and
			// each one's Select fires a move. The second would act on rows the
			// first has already moved and pruned, so the daemon would refuse it
			// with "not part of the current scan results" — true but baffling.
			// Refuse it here instead, with a reason. The flag clears in the
			// request callback, which runs on success, error and transport
			// failure alike, so it cannot outlive the move.
			if (me._moveInFlight) {
				me.notify('A move is already running — wait for it to finish.');
				return;
			}
			tool = tool || me.state.tool;
			var lower = itemNoun(tool, paths.length).toLowerCase();
			// Long moves are legitimate (an ext4 or cross-volume move copies
			// the data), so the request runs with an effectively unlimited
			// timeout behind a busy dialog that masks the app meanwhile.
			var busy = this.showBusy('Moving ' + paths.length + ' ' + lower + '…');
			// /move is one long blocking request, so its own response cannot
			// report progress. The daemon publishes per-file progress on
			// /state instead, and this polls it alongside the move. Kept
			// deliberately separate from the scan poller (pollTask): their
			// lifecycles differ and sharing the timer would tangle them.
			//
			// Nothing here may assume a move and a scan cannot be requested
			// together: this client cannot prevent it — a second DSM tab or a
			// reload mid-move reaches /api/scan directly. The daemon refuses
			// both directions (scanAdmissionLocked / beginMove).
			me._moveOrphaned = false;
			var stopPoll = function () {
				if (me._movePoll) { clearInterval(me._movePoll); me._movePoll = null; }
			};
			me._movePoll = setInterval(function () {
				api('/state', 'GET', null, function (err, st) {
					if (err || !st) return;
					me.noteSaveError(st);
					if (st.move && st.move.total) {
						busy.setProgress(st.move.done, st.move.total,
							st.move.done + ' of ' + st.move.total +
								(st.move.name ? ' — ' + st.move.name : ''));
						return;
					}
					// The request's own answer was lost (see the transport
					// branch below) and the daemon has now finished: this is
					// where the move ends for the user.
					if (me._moveOrphaned) {
						me._moveOrphaned = false;
						me._moveInFlight = false;
						stopPoll();
						busy.close();
						me.sm.clearSelections();
						me.reloadPage();
						me.notify('The move finished on the NAS and the results were refreshed. Any per-file issue is in the daemon log (dupfinder.log).');
					}
				});
			}, 700);
			me._moveInFlight = true;
			// verify defaults ON when the caller does not say otherwise — the
			// dialog always says, so this covers programmatic callers, and the
			// default lands on the safe side.
			api('/move', 'POST', { files: paths, dest: dest, preserve: preserve, tool: tool, verify: verify !== false }, function (err, j, meta) {
				if (err && meta && meta.transport) {
					// The connection died — DSM's web server gave up on the
					// request long before the 12-hour timeout here — while the
					// daemon keeps moving, holding its move lock. The outcome is
					// unknown and claiming failure would be wrong; so would
					// dropping the dialog and losing sight of a move that is
					// still running. Keep polling /state until the daemon
					// reports no move, then refresh (the poller above).
					me._moveOrphaned = true;
					me.notify('The connection was interrupted while moving — the NAS is still working on it; waiting for it to finish.');
					return;
				}
				me._moveInFlight = false;
				stopPoll();
				busy.close();
				if (err) {
					me.notify(err);
					return;
				}
				me.sm.clearSelections();
				// The daemon pruned the moved rows, so the page that is on
				// screen has shifted: reload it (onPageLoaded repaints the
				// summary, badges and totals).
				me.reloadPage();
				var n = (j.moved || []).length;
				if (j.errors && j.errors.length) {
					// A persistent dialog, never only the 3.6-second toast:
					// among these entries may sit the one message this whole
					// feature exists to produce — "the destination copy does
					// not match" — and its row has just been pruned, so this
					// listing (plus the daemon log) is the only place the
					// user can still learn WHICH file needs their attention.
					me.showMoveIssues(paths.length, n, j.errors);
					if (j.saveError) me.notify(j.saveError);
				} else {
					// Under preserve the files are NOT at `dest` — the daemon
					// created a folder inside it and may have had to number
					// it, and only the daemon knows which name it got. Report
					// what it says, never a name derived here. A save that
					// failed after the prune rides on the same toast: the
					// pruned rows would be back after a restart.
					me.notify('Moved ' + n + ' ' + itemNoun(tool, n).toLowerCase() + ' to ' + (j.folder || dest) +
						(j.saveError ? ' — ' + j.saveError : ''));
				}
			}, { timeout: LONG_OP_TIMEOUT });
		},

		// The full per-file account of a move that did not end clean. Every
		// entry is listed with its path — errors[0] alone cannot say which of
		// forty files is the one whose destination copy failed verification.
		showMoveIssues: function (requested, movedCount, errors) {
			var me = this;
			var rows = [];
			Ext.each(errors, function (e) {
				rows.push('<div style="padding:4px 0;border-bottom:1px solid rgba(128,128,128,0.25);">' +
					'<div style="word-break:break-all;">' + esc(e.path || '') + '</div>' +
					'<div style="color:' + FONT2 + ';">' + esc(e.error || '') + '</div></div>');
			});
			var win = new UxModal({
				owner: me,
				title: 'Move Issues',
				width: 560,
				height: 380,
				modal: false,
				cls: 'df-app',
				bodyStyle: 'padding:14px;font-size:' + FSMALL + ';overflow:auto;',
				html: '<b>' + movedCount + ' of ' + requested + ' moved · ' +
					errors.length + ' need' + (errors.length === 1 ? 's' : '') + ' attention.</b>' +
					'<div style="padding-top:8px;">' + rows.join('') + '</div>',
				buttons: [new UxButton({ btnStyle: 'blue', text: 'OK', handler: function () { win.close(); } })]
			});
			win.show();
		},

		openExportDialog: function () {
			var me = this;
			if (!this.state.scanned[this.state.tool]) {
				this.notify('Run a scan first');
				return;
			}
			// Captured for the same reason openMoveDialog captures it: the tool
			// the user asked to export is the one that should be exported, not
			// whatever state.tool says when the chooser finally returns. DSM
			// masks the app for the chooser's lifetime so the rail cannot be
			// clicked meanwhile, but that is DSM's behaviour to change, not
			// ours to rely on — and an export names its tool in the CSV.
			var tool = this.state.tool;
			pickFolder({
				owner: this,
				onError: function (msg) { me.notify(msg); },
				title: 'Export Results to…',
				onPick: function (dest) {
					// A big report over a slow box can pass 30 s: same long
					// timeout + busy treatment as moves.
					var busy = me.showBusy('Exporting…',
						'Writing the CSV report through File Station. Nothing is overwritten — an existing report gets a numbered sibling.');
					api('/export', 'POST', { tool: tool, dest: dest }, function (err, j, meta) {
						busy.close();
						if (err) {
							me.notify(meta && meta.transport
								? 'The connection was interrupted while exporting — the report may still have been written. Check the destination folder.'
								: err);
							return;
						}
						me.notify('Results exported to ' + j.file);
					}, { timeout: LONG_OP_TIMEOUT });
				}
			});
		},

		openFileStation: function (path) {
			try {
				// "opendir" (share-space path) is File Station's supported way
				// to open a folder. Its "openfile" highlight variant is not
				// reliable: FS applies the highlight one-shot against a
				// variable number of internal list loads, so it only sticks
				// sometimes — opening the folder is the reliable behavior.
				SYNO.SDS.AppLaunch('SYNO.SDS.App.FileStation3.Instance',
					{ opendir: realToShare(path) }, true);
			} catch (e) {
				this.notify('Open ' + path + ' in File Station');
			}
		}
	});

	/* ------------------------------------------------------ app instance */
	SYNO.SDS.DuplicateFinder.Application = Ext.extend(SYNO.SDS.AppInstance, {
		appWindowName: 'SYNO.SDS.DuplicateFinder.MainWindow',
		constructor: function () {
			SYNO.SDS.DuplicateFinder.Application.superclass.constructor.apply(this, arguments);
		}
	});

})();
