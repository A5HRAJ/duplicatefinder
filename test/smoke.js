/* Headless smoke test for the native ExtJS DSM app.
   Loads harness.html (ExtJS 3.4 + SYNO.SDS stubs + real DuplicateFinder.js)
   against the live dev daemon and drives the main window. */
const { JSDOM } = require('jsdom');
const fs = require('fs');
const path = require('path');

const PORT = process.env.DUPFINDER_TEST_PORT || 9807;
const BASE = 'http://localhost:' + PORT + '/harness.html';
let failures = 0;
function check(name, cond) {
  console.log((cond ? 'PASS' : 'FAIL') + '  ' + name);
  if (!cond) failures++;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// A scan reports "scanned" as soon as its totals arrive; the grid's first
// page is a further round trip (slow under QEMU). Waiting for "some rows"
// is not enough — the store still holds the previous tool's page until the
// new one lands — so wait for a load that happened after a given point.
async function waitForLoadAfter(win, seqBefore, min, tries) {
  for (let i = 0; i < (tries || 80); i++) {
    if (win.__loads > seqBefore && win.store.getCount() >= (min || 1)) return true;
    await sleep(200);
  }
  return false;
}

(async () => {
  const virtualConsole = new (require('jsdom').VirtualConsole)();
  virtualConsole.on('jsdomError', (e) => {
    // css parse errors etc. are noise; script errors matter
    if (String(e.message || e).indexOf('Could not parse CSS') < 0) {
      console.log('jsdomError: ' + (e.message || e));
    }
  });
  // harness.html has to name the daemon's origin in the <script src> for the
  // app bundle, so it carries the default port literally. Served verbatim it
  // would fetch the UI from 9807 whatever DUPFINDER_TEST_PORT says — and the
  // resulting "MainWindow is undefined" reads like an app regression, not a
  // wiring mistake. Read the file, retarget every absolute daemon URL at the
  // port actually in use, and hand jsdom the string with `url` set so the
  // relative ext-*.js srcs still resolve against the daemon.
  const harness = fs.readFileSync(path.join(__dirname, 'harness.html'), 'utf8')
    .replace(/localhost:9807\b/g, 'localhost:' + PORT);
  if (String(PORT) !== '9807' && /\b9807\b/.test(harness)) {
    throw new Error('harness.html still points at port 9807 after substitution; ' +
      'add the new form to the rewrite in smoke.js');
  }
  const dom = new JSDOM(harness, {
    url: BASE,
    runScripts: 'dangerously',
    resources: 'usable',
    pretendToBeVisual: true,
    virtualConsole,
  });
  const { window } = dom;
  const doc = window.document;

  await sleep(3000); // ext load + onReady + bootstrap (info/state fetches)

  const errs = window.__harnessErrors || ['harness never initialized'];
  check('no window errors during construct/show: ' + JSON.stringify(errs.slice(0, 2)), errs.length === 0);
  const win = window.__win;
  check('main window constructed', !!win);
  if (!win) { console.log(failures + ' FAILURES'); process.exit(1); }
  check('main window rendered', !!win.rendered);
  // The real SYNO.SDS.AppWindow pushes a "?" tool next to minimize unless
  // showHelp is already false when its constructor runs; it opens DSM Help on
  // this app's jsID, where a 3rd-party package has no page. The harness stubs
  // AppWindow as a plain Ext.Window, so only the flag can be asserted here —
  // the button's actual absence is checked on-device.
  check('DSM Help button suppressed (showHelp false)', win.showHelp === false);
  // count page loads so the assertions can wait for the right one
  win.__loads = 0;
  win.store.on('load', function () { win.__loads++; });
  check('rail shows 5 tools', doc.querySelectorAll('.df-rail-item').length === 5);
  check('each rail item has an SVG icon (not escaped text)', doc.querySelectorAll('.df-rail-item svg.df-rail-ico').length === 5);
  check('selected rail row uses DSM-themed class (inherits highlight)', doc.querySelectorAll('.df-rail-item.x-grid3-row-selected').length === 1);
  check('rail item labels present alongside icons', /Duplicate Files/.test((doc.querySelector('.df-rail-label') || {}).textContent || ''));
  // The read-only category is "Conflicting Files": "Corrupted" would assert of
  // every row what the scan can only prove of a few, so it survives as the
  // per-row verdict only, which is why the labels are checked separately
  // below. Its icon is a warning triangle: a red stroked+filled triangle path
  // with a white bar and dot.
  {
    const railLabels = [...doc.querySelectorAll('.df-rail-label')].map((el) => el.textContent.trim());
    check('the read-only category reads "Conflicting Files"',
      railLabels.includes('Conflicting Files') && !railLabels.some((l) => /Corrupted/.test(l)));
    const rows = [...doc.querySelectorAll('.df-rail-item')];
    const conflictRow = rows.find((r) => /Conflicting Files/.test(r.textContent));
    const ico = conflictRow && conflictRow.querySelector('svg.df-rail-ico');
    const paths = ico ? [...ico.querySelectorAll('path')] : [];
    check('its icon is a red warning triangle with an exclamation mark',
      !!ico && paths.length === 2 &&
      /^M12 4\.6 L20\.2 19 H3\.8 Z$/.test(paths[0].getAttribute('d')) &&
      paths[0].getAttribute('fill') === '#e2766c' &&
      paths[0].getAttribute('stroke-linejoin') === 'round' &&   // rounds the 3 corners
      paths[1].getAttribute('stroke') === '#fff' &&             // the "!" bar
      !!ico.querySelector('circle[fill="#fff"]'));              // the "!" dot
  }

  // Toast goes through DSM's SYNO.SDS.ToastBox (harness stubs it). Assert the
  // DSM branch actually runs — with no stub the app silently falls back to its
  // own .df-toast div and this whole area would test nothing.
  const winCountBefore = doc.querySelectorAll('.x-window').length;
  const toastsBefore = window.__toasts || 0;
  win.notify('toast check message');
  await sleep(60);
  check('notify uses DSM ToastBox, not the fallback element',
    (window.__toasts || 0) === toastsBefore + 1 &&
    window.__lastToastText === 'toast check message' &&
    !doc.querySelector('.df-toast'));
  check('toast is app-scoped, not a new Ext window',
    doc.querySelectorAll('.x-window').length === winCountBefore);
  // Regression guard: passing `listeners` clobbers ToastBox's own, leaving a
  // toast that never auto-dismisses and whose close button does nothing.
  check('notify does not pass listeners to ToastBox (would break dismiss + X)',
    window.__lastToastCfgHadListeners === false);
  // a second notify must replace the first, never stack
  win.notify('second toast');
  await sleep(20);
  check('a second notify replaces the first', (window.__toasts || 0) === toastsBefore + 2);
  win.dismissToast();
  check('dismissToast clears the toast reference', !win._toast && !win._toastEl);

  check('checkbox column widened to 32px', win.sm.width === 32);
  // Volume card: DSM's PercentageBar markup + DSM's CapacityRender units
  const volHtml = (doc.querySelector('.df-vol') || {}).innerHTML || '';
  check('volume card rendered', /Volume 1/.test(volHtml));
  check('usage bar is DSM PercentageBar, not the hand-drawn div',
    /percentage-cmp-hbar-fill/.test(volHtml) &&
    /sds-ux-progressbar/.test(volHtml) &&
    !/df-vol-fill/.test(volHtml));
  check('capacities rendered through DSM CapacityRender',
    /\d+(\.\d)? (MB|GB|TB) of \d+(\.\d)? (MB|GB|TB) used/.test(volHtml));
  check('default scope populated from shares', win.state.dirs.length >= 1);
  check('idle card active before scan', win.centerPanel.getLayout().activeItem === win.idlePanel);

  // progress dialog structure (driven directly so it doesn't race the scan):
  // the phase label must be its own rendered element above the bar, not text
  // painted inside the bar (which clips at the bar's height).
  win.attachProgress();                  // renders synchronously on show()
  window.Ext.TaskMgr.stop(win.pollTask); // freeze poll so it can't tear down mid-check
  const bar = win.progressBar, label = win.progressLabel;
  const barEl = bar.getEl(), labelEl = label.body;
  check('progress bar and label are distinct components', bar !== label);
  check('progress label is rendered outside the bar', !!labelEl && !barEl.dom.contains(labelEl.dom));
  label.body.update('Computing Blake3 hashes  (40%)');
  check('label holds the full phase text', /Computing Blake3 hashes/.test(label.body.dom.innerHTML));
  // The dialog must not be able to slip behind the app window. That is DSM's
  // job, not ours: a SYNO.SDS.ModalWindow given `owner` is registered in
  // owner.modalWin by afterShow, and a mousedown on the owner's mask runs
  // blinkModalChild, which raises the topmost modal child and flashes it.
  // The harness stubs ModalWindow as a plain Ext.Window, so the z-order
  // behaviour itself CANNOT be tested here — it is verified on-device against
  // File Station's own Settings dialog. What jsdom can pin is the config that
  // hands the job to DSM: hand-rolling this with an 'activate' listener plus a
  // raw getEl().mask() leaves the app window on top of the dialog anyway.
  const pw = win.progressWin;
  check('progress dialog is owned by the app window (DSM keeps it on top)',
    pw.owner === win);
  win.stopPolling();
  check('teardown clears progress refs', win.progressWin === null && win.progressBar === null);

  // drive a duplicates scan through the real backend
  const dupSeq = win.__loads;
  win.startScan();
  for (let i = 0; i < 40 && !win.state.scanned.duplicates; i++) await sleep(300);
  check('scan completed and results stored', !!win.state.scanned.duplicates);
  // Wait for the ROWS, not just the flag, before asserting on the card:
  // state.scanned[tool] flips when fetchMeta's totals arrive, one round trip
  // BEFORE the first page — so checking the active card here raced the load
  // and failed intermittently (seen once while adding the nomove column, which
  // shifted timing by a few bytes per row).
  await waitForLoadAfter(win, dupSeq, 4);
  check('grid card active after scan', win.centerPanel.getLayout().activeItem === win.grid);
  check('grid has rows', win.store.getCount() >= 4);

  // Paged delivery, File Station style: the fixture holds 107 duplicate
  // groups and a page is 100 groups, so the grid shows page 1 of 2 under
  // DSM's own pager.
  const dupRes = win.state.results.duplicates;
  check('paged fetch stores whole-set totals', !!dupRes.total &&
    dupRes.total.groups === 107 && dupRes.total.files === 214);
  check('first page holds 100 whole groups', win.pageUnitCount() === 100);
  check('pager counts groups for duplicates', win.store.getTotalCount() === 107);
  check('pager is the grid bottom bar, where File Station puts it',
    win.grid.getBottomToolbar() === win.pager);
  check('pager sized to the group page size', win.pager.pageSize === 100);
  check('pager reports the count in groups', win.pager.displayMsg === '{2} groups');
  // DSM decides nav visibility from total-vs-loaded, which counts groups
  // against rows here — the override makes it the page count instead, so a
  // 107-group / 2-page listing keeps its page buttons.
  // (getPageData is stock Ext; getTotalPage is DSM's wrapper around it)
  check('page navigation stays visible when groups < rows',
    win.pager.getPageData().pages === 2 && win.store.getTotalCount() < win.store.getCount());
  // (Ext 3.4's TextItem.setText writes the element once rendered and leaves
  // .text stale, so read the rendered element.)
  const summaryHtml = win.summaryText.el.dom.innerHTML;
  check('summary reports whole-set totals and no loaded-count clause (' + summaryHtml.slice(0, 80) + ')',
    /214 duplicate files<\/b> in 107 groups/.test(summaryHtml) && !/showing first/.test(summaryHtml));
  /* The top toolbar has ~2px spare at the window's minWidth once the longest
     match label is showing, so the summary's width is load-bearing: measured
     on-device, an exact count overflows the search box off the window by 40px
     at 34k duplicates and 132px at 1.2M + "capped". jsdom does no layout, so
     the guard is on the two things that bound the width — compact counts above
     10k, and the capped wrapper that clips whatever is left. */
  check('summary is wrapped in the width-capped span with a full-text tooltip',
    /class="df-summary"/.test(summaryHtml) && /ext:qtip=/.test(summaryHtml));
  {
    // drive the real summary builder, not a copy of its formatting rules
    const savedTotals = win.state.results.duplicates;
    const render = (files, groups) => {
      win.state.results.duplicates = { total: { files, groups, reclaimable: 1 << 30 } };
      win.summaryTextForDup();
      return win.summaryText.el.dom.innerHTML;
    };
    check('summary counts stay exact below 10k', /9,999 duplicate files/.test(render(9999, 4000)));
    check('summary counts go compact at 10k and above',
      /34\.2K duplicate files/.test(render(34182, 12905)) &&
      /513K groups/.test(render(999999, 512847)) &&
      /1\.28M duplicate files/.test(render(1284663, 512847)));
    win.state.results.duplicates = savedTotals;
    win.summaryTextForDup();
  }
  check('grid view uses forceFit (File Station-style column sizing)', win.grid.getView().forceFit === true);
  check('rows carry group labels', /identical files/.test(win.store.getAt(0).get('grpLabel')));
  check('results carry scan-time match criteria', !!win.state.results.duplicates.scanMatch);
  check('summary text set', /duplicate files/.test(win.summaryText.getEl ? doc.body.innerHTML : ''));
  check('badge on duplicates tool', /\d/.test(win.toolsStore.getAt(0).get('badge')));

  // The checker's className must be EXACTLY the stock string. Ext's
  // CheckboxSelectionModel.onMouseDown tests `t.className ==
  // "x-grid3-row-checker"` by string equality, so any second class silently
  // stops every checkbox click from toggling selection. Adding
  // syno-ux-checkbox-icon here would do exactly that; DSM's checkbox art is
  // applied by URL from the stylesheet instead.
  const checkers = [...doc.querySelectorAll('.x-grid3-row-checker')];
  check('checkers exist', checkers.length >= 4);
  check('checker className is exactly the stock string (Ext matches it by ==)',
    checkers.every((el) => el.className === 'x-grid3-row-checker'));
  // NOTE: a synthetic mousedown on the checker does NOT exercise Ext's
  // selection wiring under jsdom (tried; it selects nothing either way, so the
  // assertion passes vacuously and proves nothing). The className check above
  // is the meaningful automated guard; the click path itself has to be
  // confirmed on-device.
  const cssText = window.document.getElementById('duplicate-finder-css') ?
    window.document.getElementById('duplicate-finder-css').textContent : '';
  // DSM centres a 20px box in a 28px row with margin-top, not vertical-align;
  // vertical-align:middle would inflate rows to 29.5px against DSM's 28px.
  check('checker centred DSM\'s way, not by vertical-align',
    /x-grid3-row-checker\{[^}]*margin-top:4px/.test(cssText) &&
    !/td\.x-grid3-td-checker\{vertical-align:middle/.test(cssText));
  // Unselectable rows must NOT be greyed. Grey text is this app's
  // "Missing"/"Loading" signal; spending it on protection would make two
  // unrelated states look alike. The disabled checkbox and the padlock carry
  // it instead.
  check('unselectable rows keep normal text colour',
    !/df-row-prot \.x-grid3-cell-inner/.test(cssText) &&
    !/df-row-capped \.x-grid3-cell-inner/.test(cssText));
  // A capped row is an ordinary candidate that is merely blocked while it is
  // the last one left, so it keeps a box in DSM's disabled state (-40px). A
  // protected row is never a candidate and gets a padlock instead of a box.
  // The -120px grayed row of the sprite is DSM's tri-state parent and means
  // neither.
  check('checker states use DSM sprite offsets',
    /row-selected \.x-grid3-row-checker\{background-position:0 -60px/.test(cssText) &&
    /df-row-capped \.x-grid3-row-checker\{background-position:0 -40px/.test(cssText));
  check('no row uses DSM\'s tri-state grayed checkbox',
    !/background-position:0 -120px/.test(cssText));
  // The padlock is centred the same way the checker is (20px box, 28px row), or
  // it grows the row it sits in.
  // line-height pins the glyph inside that box: an emoji font's natural line
  // box is taller than 20px and would push the row open on its own.
  check('protected-row padlock keeps DSM\'s 20-in-28 centring',
    /df-prot-lock\{[^}]*width:20px[^}]*height:20px[^}]*margin-top:4px/.test(cssText) &&
    /df-prot-lock\{[^}]*line-height:20px/.test(cssText));
  // The padlock glyph is Unicode, not hand-drawn artwork. The grid must carry
  // no inline SVG at all: an SVG element's
  // .className is an SVGAnimatedString, and Ext's getTarget/QuickTips both
  // assume a string, so an SVG node inside a row is a live event-target hazard.
  // The only inline SVG left in the app is the left rail's module icon
  // (df-rail-ico), which stays. Counting occurrences is the guard: it fails
  // if any row artwork is hand-drawn.
  const appSrc = fs.readFileSync(
    path.join(__dirname, '..', 'spk', 'ui', 'DuplicateFinder.js'), 'utf8');
  const svgOpens = appSrc.match(/'<svg/g) || [];
  check('no hand-drawn protection artwork survives',
    !/df-prot-shield/.test(cssText) && !/df-prot-shield/.test(appSrc) &&
    !/df-prot-lock svg/.test(cssText) &&
    svgOpens.length === 1 && /df-rail-ico/.test(appSrc));

  // Guard for the standing rule that DSM paints the grid: measured against
  // File Station, these overrides are redundant or actively divergent, and
  // introducing any of them is a bug.
  check('no grid overrides fighting DSM\'s theme',
    !/x-grid3-hd-inner\{/.test(cssText) &&        // 11px bold vs FS's 13px/400
    !/x-grid3-header\{background/.test(cssText) && // #f8f9fb vs FS's transparent
    !/scroll-menu-ct/.test(cssText) &&             // only ever needed because of the above
    !/x-grid3-row-over\{background/.test(cssText) &&
    !/x-grid3-row-selected\{background/.test(cssText) &&
    !/x-grid-group-hd/.test(cssText) &&
    !/x-grid3-scroller\{overflow-x/.test(cssText));
  // The app must reference DSM's design tokens rather than guessed hex values.
  check('chrome colours come from DSM tokens',
    /var\(--v-color-border-tier3/.test(cssText) &&
    /var\(--v-color-action-normal/.test(cssText) &&
    !/#0086e5/.test(cssText) &&                    // DSM 6's blue; DSM 7.4 is #057FEB
    !/#0070bf/.test(cssText));
  // The blanket "toolbars have no borders" rule must not swallow the divider
  // DSM draws between the rows and the paging strip (File Station has it).
  check('paging strip keeps DSM\'s divider above it',
    /x-toolbar\.syno-ux-pagingtoolbar\{border-top-width:1px/.test(cssText) &&
    /x-panel-bbar \.x-toolbar\{border-top-width:0/.test(cssText));

  // selection flows
  win.autoSelect('newest');
  const sel1 = win.sm.getSelections().length;
  check('keep-newest selected rows (' + sel1 + ')', sel1 >= 2);
  check('move button enabled with selection', !win.moveBtn.disabled);
  win.sm.clearSelections();
  check('deselect clears', win.sm.getSelections().length === 0);
  check('move button disabled without selection', win.moveBtn.disabled);

  // The search box must be DSM's SearchField, and must NOT pass its own
  // emptyText — SearchField installs a localized "Search" placeholder that our
  // hardcoded English string would silently overwrite.
  check('search box is DSM SearchField', win.searchField instanceof window.SYNO.ux.SearchField);
  check('search placeholder left to DSM to localize',
    win.searchField.emptyText === '__localized_search__');

  // search box: server-side filter (the grid holds one page, so the daemon
  // must search the whole set) — and it re-pages from the first page
  const totalRows = win.store.getCount();
  win.state.query = 'img_0001';
  win.applySearch();
  for (let i = 0; i < 30 && win.store.getCount() !== 2; i++) await sleep(150);
  let searchMiss = false;
  win.store.each((r) => { if (!/IMG_0001/i.test(r.get('name'))) searchMiss = true; });
  check('search narrows rows to matches', win.store.getCount() === 2 && !searchMiss && totalRows > 2);
  check('pager total follows the filtered set', win.store.getTotalCount() === 1);
  // Clear the way a USER does — the field's X trigger, which calls
  // setValue('') + filter() and fires NO key event. Driving state.query
  // directly here would let keyup-only wiring pass, and with that wiring
  // state.query stays stale and the old filtered page stays on screen.
  win.searchField.setValue('');
  win.searchField.filter();
  for (let i = 0; i < 30 && win.store.getCount() === 2; i++) await sleep(150);
  check('clearing via the X restores the first page', win.store.getCount() === totalRows &&
    win.store.getTotalCount() === 107 && win.state.query === '');

  // --- magnifier menu (File Station's advanced search options) ---
  const menu = win.getSearchMenu();
  check('magnifier menu builds once', !!menu && win.getSearchMenu() === menu);
  const f = win.soFields;
  check('menu offers FS option set',
    f.type.store.getCount() === 12 && f.sizeOp.store.getCount() === 4 &&
    f.dateBy.store.getCount() === 3);

  // Picking any option must not close the whole menu and wipe the rest. Two
  // independent causes can do that, both guarded here because jsdom does no
  // layout and cannot reproduce the click itself.
  //  (a) combo dropdowns render to document.body -> MenuMgr reads the list
  //      click as "outside" and hides the menu. getListParent must return the
  //      menu element instead.
  check('combo dropdowns are scoped to the menu, not document.body',
    ['loc', 'type', 'dateBy', 'sizeOp'].every((k) =>
      f[k].getListParent !== window.Ext.form.ComboBox.prototype.getListParent));
  // documents WHY the override is needed — the stock hook renders to body
  check('stock getListParent renders to body (the cause)',
    /document\.body/.test(String(window.Ext.form.ComboBox.prototype.getListParent)));
  //  (b) DateField shows its picker with show(el, pos) and no parentMenu, so
  //      MenuMgr treats our menu as unrelated and hides it. The picker must be
  //      pre-built and bound to name the search menu as its parent.
  check('date pickers are pre-bound to the search menu',
    !!f.from.menu && !!f.to.menu &&
    f.from.menu !== f.to.menu &&
    String(f.from.menu.show).indexOf('stockShow') >= 0);
  // Must be DSM's own picker, configured like File Station's: a stock
  // Ext.menu.DateMenu here loses the year/month dropdowns and the Clear
  // button. Prefer DateTimeField/DateTimeMenu, and keep DSM's menu class.
  check('date fields are DSM\'s File Station picker, not the stock one',
    f.from instanceof window.SYNO.ux.DateTimeField &&
    f.from.menu instanceof window.SYNO.ux.DateTimeMenu &&
    f.from.menu.cls === 'syno-ux-datefield-menu');
  check('date fields configured like File Station\'s',
    f.from.editable === false && f.from.format === 'Y-m-d' &&
    f.from.emptyText === 'From' && f.to.emptyText === 'To');

  // File Station's row grouping: paired controls share a composite row.
  {
    const rows = win.searchMenu.items.get(0).items;
    const composites = [];
    rows.each((it) => { if (it instanceof window.SYNO.ux.CompositeField) composites.push(it); });
    check('paired controls share composite rows like File Station',
      composites.length === 3 &&
      composites[0].items.get(0) === f.type && composites[0].items.get(1) === f.ext &&
      composites[1].items.get(0) === f.from && composites[1].items.get(1) === f.to &&
      composites[2].items.get(0) === f.sizeOp && composites[2].items.get(1) === f.size);
    check('composite children use flex with FS\'s 8px gutter',
      f.type.flex === 1 && f.ext.flex === 1 && f.type.margins === '0 8 0 0');
    check('the From/To row carries no label, like File Station\'s',
      composites[1].hideLabel === true && composites[0].fieldLabel === 'File Type');
  }


  // The year/month dropdowns inside DSM's picker are menus parented to the
  // picker, and Ext cascades a DEEP hide up the parentMenu chain — which took
  // the picker AND the whole search menu down when a year was chosen. The
  // picker must refuse a deep hide while still closing on a normal one.
  // Behavioural, using the same wrapper shape the app installs: a deep hide is
  // swallowed, a plain one delegates. (The real picker's wrapper closes over
  // its stock hide, so it cannot be spied on directly; the on-device check is
  // what proves the wired-up widget — year keeps it open, day closes it.)
  {
    let delegated = 0;
    const fake = { hide: function () { delegated++; return this; } };
    const stock = fake.hide;
    fake.hide = function (deep) {
      if (deep === true) return this;
      return stock.apply(this, arguments);
    };
    fake.hide(true);
    check('a deep hide is swallowed (the year-dropdown cascade)', delegated === 0);
    fake.hide();
    check('a plain hide still closes it (day click, outside click)', delegated === 1);
    // and the app really does install that guard on both pickers
    check('both pickers carry the deep-hide guard',
      [f.from, f.to].every((fld) =>
        fld.menu.hasOwnProperty('hide') && /deep === true/.test(String(fld.menu.hide))));
  }

  // The pickers are limited by the DATA (oldest/newest file for the selected
  // date field) and by EACH OTHER (no To before From). Both together.
  {
    const range = win.state.results.duplicates.dateRange;
    check('daemon reports a whole-scan date span',
      !!range && !!range.modified.min && !!range.modified.max &&
      range.modified.min <= range.modified.max);
    const day = (v) => window.Date.parseDate(String(v).substring(0, 10), 'Y-m-d');
    const oldest = day(range.modified.min), newest = day(range.modified.max);

    f.from.setValue(''); f.to.setValue('');
    f.dateBy.setValue('modified');
    win.refreshDateBounds();
    check('From cannot go back before the oldest file',
      +f.from.minValue === +oldest);
    check('To cannot go forward past the newest file',
      +f.to.maxValue === +newest);

    // ...and the two still constrain each other within that span
    // window.Date, not Node's: Ext decorates the jsdom Date prototype with
    // dateFormat, and setValue() calls it
    const mid = new window.Date(+oldest + Math.floor((+newest - +oldest) / 2));
    // Midnight, like oldest/newest: the field stores its value as m/d/Y text
    // and getValue() parses it back, so any time-of-day here is dropped on the
    // round trip and the comparison below would miss by hours. (Invisible
    // while the fixture's files all shared one timestamp and the span was
    // zero-width.)
    mid.setHours(0, 0, 0, 0);
    f.from.setValue(mid);
    win.refreshDateBounds();
    check('picking From raises the To picker\'s minimum', +f.to.minValue === +mid);
    check('To still cannot exceed the newest file', +f.to.maxValue === +newest);
    f.to.setValue(newest);
    win.refreshDateBounds();
    check('picking To lowers the From picker\'s maximum', +f.from.maxValue === +newest);
    check('From still cannot precede the oldest file', +f.from.minValue === +oldest);

    // clearing a side falls back to the data bound, not to DSM's wide default
    f.from.setValue(''); f.to.setValue('');
    win.refreshDateBounds();
    check('clearing restores the data bounds, not 1990/2060',
      +f.to.minValue === +oldest && +f.from.maxValue === +newest);

    // With nothing chosen, each picker should OPEN on its end of the data —
    // From on the oldest file's month, To on the newest — rather than today.
    // It must move the displayed month only: setValue() would fill the field
    // with a date the user never picked, so the wrapper uses update().
    // (The trigger wrapper itself needs a rendered field+menu, which jsdom
    // cannot provide, so the decision lives in pickerOpenDate and is tested
    // directly; the wrapper only feeds it to picker.update.)
    check('From opens on the oldest file\'s month',
      +win.pickerOpenDate('lo', false) === +oldest);
    check('To opens on the newest file\'s month',
      +win.pickerOpenDate('hi', false) === +newest);
    check('a chosen date is left to seed the picker itself',
      win.pickerOpenDate('lo', true) === null && win.pickerOpenDate('hi', true) === null);
    check('the picker is only re-aimed, never given a value',
      /picker\.update\(d\)/.test(String(win.bindDatePicker)) &&
      !/picker\.setValue/.test(String(win.bindDatePicker)));
    // Clear must land back on the opening month, not today. The button lives
    // inside the rendered picker, so the wiring itself can only be checked
    // on-device; what is asserted here is that Clear is wired at all and that
    // it reuses pickerOpenDate (so it can never drift from the open month).
    check('Clear is wired to return to the opening month',
      /__clearHooked/.test(String(win.bindDatePicker)) &&
      /clear-btn/.test(String(win.bindDatePicker)) &&
      /pickerOpenDate\(end, false\)/.test(String(win.bindDatePicker)) &&
      /picker\.update\(back\)/.test(String(win.bindDatePicker)));
    // Clear fires no select event, so it must recompute the pair's bounds
    // itself — otherwise the OTHER field stays limited by the date just
    // removed, and From claims "must be <date> or earlier" long after that To
    // date has been cleared.
    check('Clear recomputes the pair\'s bounds',
      /refreshDateBounds\(\)/.test(String(win.bindDatePicker)));
    {
      // simulate exactly that sequence: To set, then cleared
      f.from.setValue(''); f.to.setValue('');
      win.refreshDateBounds();
      const early = new window.Date(+oldest + 86400000);
      f.to.setValue(early);
      win.refreshDateBounds();
      check('a To date caps the From field', +f.from.maxValue === +early);
      f.to.setValue('');            // what Clear leaves behind
      win.refreshDateBounds();      // what Clear must now trigger
      check('clearing To releases the From cap back to the newest file',
        +f.from.maxValue === +newest);
      // and a value that was illegal under the stale cap is no longer flagged
      f.from.setValue(newest);
      win.refreshDateBounds();
      check('the freed field is re-validated, not left showing an error',
        f.from.isValid() === true);
      f.from.setValue(''); f.to.setValue('');
      win.refreshDateBounds();
    }

    /* The picker is PRE-BUILT so show() can bind parentMenu before the first
       click — but DateField wires its select relay inside the same lazy branch
       that creates the menu, so assigning field.menu skips it and a chosen day
       never reaches the field: the day highlights, the value is never set, the
       menu never closes. Asserted behaviourally rather than by grepping the
       source, because a source regex cannot see that. */
    {
      f.from.setValue(''); f.to.setValue('');
      win.refreshDateBounds();
      const day = new window.Date(+oldest + 3 * 86400000);
      // what a real day click produces: the MENU fires select
      f.from.menu.fireEvent('select', f.from.menu, day);
      check('a day chosen in the picker reaches the field',
        +f.from.getValue() === +day);
      check('choosing a day applies the two-way constraint',
        +f.to.minValue === +day);
      f.from.setValue(''); f.to.setValue('');
      win.refreshDateBounds();
    }

    /* Opening month. DSM's own deferred updateDateTime calls setValue('') ->
       update(''), and an empty date makes the picker fall back to today —
       after the trigger wrapper has already aimed it, so only the FIRST open
       was right and every later one showed today (with every day disabled,
       since today is past maxValue). The fix makes the empty-date fallback
       itself correct, which is timing-independent; assert that update('')
       lands on the data's month and still writes no value. */
    {
      // Ext's DatePicker.update is a no-op until the picker is rendered, so the
      // menu has to be shown for this to exercise anything at all.
      // menu.show() needs a positioned owner element jsdom cannot give it, so
      // render the picker on its own — update() only touches activeDate and the
      // day cells, both of which exist once rendered.
      const aim = (field) => {
        field.setValue('');
        win.refreshDateBounds();
        const p = field.menu.picker;
        if (!p.rendered) p.render(doc.body);
        p.update('');                       // DSM's deferred setValue('') path
        return { at: p.activeDate, rendered: p.rendered, committed: field.getValue() };
      };
      const from = aim(f.from);
      check('the picker renders in the harness (else the check below is vacuous)',
        from.rendered === true);
      check('an empty picker update opens on the oldest file\'s month',
        !!from.at && from.at.getFullYear() === oldest.getFullYear() &&
        from.at.getMonth() === oldest.getMonth());
      check('aiming the picker never fills the field', !from.committed);
      const to = aim(f.to);
      check('the To picker opens on the newest file\'s month',
        !!to.at && to.at.getFullYear() === newest.getFullYear() &&
        to.at.getMonth() === newest.getMonth());
      check('aiming the To picker never fills the field', !to.committed);
      f.from.setValue(''); f.to.setValue('');
      win.refreshDateBounds();
    }

    // a field no file carries falls back to DSM's own wide defaults
    const savedRange = win.state.results.duplicates.dateRange;
    win.state.results.duplicates.dateRange = { modified: {}, created: {}, captured: {} };
    win.refreshDateBounds();
    check('a date field with no data falls back to DSM\'s defaults',
      f.from.minValue === win._dateBounds.toMin && f.to.maxValue === win._dateBounds.fromMax);
    win.state.results.duplicates.dateRange = savedRange;
    win.refreshDateBounds();
  }

  // Binding the picker to the menu (so the menu survives) stopped the picker
  // auto-closing when the user moved to another field, leaving two popups open
  // at once. closeDatePickers must shut it for a click elsewhere in the menu,
  // but NOT for clicks inside the picker (its month/year dropdowns live there)
  // nor on its own trigger.
  {
    let hidden = 0;
    const fake = (visible) => ({
      isVisible: () => visible,
      hide: () => { hidden++; },
      getEl: () => ({ dom: { contains: (t) => t === 'INSIDE_PICKER' } })
    });
    const realFrom = f.from.menu, realTo = f.to.menu;
    f.from.menu = fake(true); f.to.menu = fake(false);
    win.closeDatePickers('SOME_OTHER_FIELD');
    check('picker closes when another field is clicked', hidden === 1);
    hidden = 0;
    win.closeDatePickers('INSIDE_PICKER');
    check('picker survives clicks inside itself (month/year dropdowns)', hidden === 0);
    // NOT covered here: a click on the field's OWN trigger. That path needs a
    // rendered .x-form-field-wrap ancestor, and the menu never renders under
    // jsdom, so the assertion would pass vacuously. Verified on-device instead.
    f.from.menu = realFrom; f.to.menu = realTo;
  }
  // NOTE: this must stay AFTER the closeDatePickers test above, which relies on
  // the menu's fields being unrendered — showing the menu renders them.
  // Reset/Search wear File Station's pill shape. DSM's base .syno-ux-button is
  // a 3px rectangle and DSM re-rounds it only for buttons in a WINDOW footer,
  // which an Ext menu is not — so the app supplies one scoped rule. jsdom does
  // no layout, so this asserts what jsdom CAN decide: that the shipped selector
  // actually matches the rendered button, and that it does not reach the
  // toolbar buttons, which must stay square like DSM's own. An earlier cut of
  // this rule used .x-toolbar and silently matched nothing.
  {
    // Read the selector the app actually SHIPS, rather than restating it here:
    // a hardcoded copy would keep passing while the real rule matched nothing.
    // Parsed from the injected stylesheet's raw text — jsdom's own CSS parser
    // does not expose these rules.
    const cssText = Array.from(doc.querySelectorAll('style'))
      .map((el) => el.textContent || '').join('\n');
    const pillRules = [];
    cssText.replace(/([^{}]*syno-ux-button[^{}]*)\{([^}]*)\}/g, (m, sel, body) => {
      if (/border-radius\s*:\s*100px/.test(body)) pillRules.push(sel.trim());
      return m;
    });
    check('the app ships exactly one button pill rule (' + JSON.stringify(pillRules) + ')',
      pillRules.length === 1);
    const PILL_SEL = pillRules[0] || '.df-no-such-rule-was-shipped';
    const uxAncestor = (btn) => {
      let s = btn;
      while (s && !/syno-ux-button/.test(s.className || '')) s = s.parentElement;
      return s;
    };
    win.searchMenu.show(win.searchField.getEl());
    const menuEl = win.searchMenu.getEl().dom;
    check('the search menu carries the class the pill rule is scoped to',
      /\bdf-search-menu\b/.test(menuEl.className));
    const footer = ['Reset', 'Search'].map((label) => {
      const b = Array.from(menuEl.querySelectorAll('button'))
        .find((x) => (x.textContent || '').trim() === label);
      return b ? uxAncestor(b) : null;
    });
    check('both footer buttons rendered', footer.every(Boolean));
    check('the pill rule actually matches Reset and Search',
      footer.every((el) => el.matches(PILL_SEL)));
    const toolbarBtn = Array.from(win.getEl().dom.querySelectorAll('button'))
      .map(uxAncestor).filter(Boolean)
      .find((el) => !menuEl.contains(el));
    check('the pill rule does not reach the toolbar buttons',
      !!toolbarBtn && !toolbarBtn.matches(PILL_SEL));
    win.searchMenu.hide();
  }

  // in-progress edits survive a dismissal that is not an apply
  win._soSynced = true;
  f.ext.setValue('typed-but-not-applied');
  win.syncSearchMenu();
  check('reopening does not discard un-applied edits',
    f.ext.getValue() === 'typed-but-not-applied');
  win.resetSearchOpts();
  win.syncSearchMenu();
  check('after Reset the menu re-reads cleared state', f.ext.getValue() === '');
  // Pictures → only the IMG_0001.JPG group, whole, server-side
  f.type.setValue('pic');
  win.applySearchMenu();
  for (let i = 0; i < 30 && win.store.getTotalCount() !== 1; i++) await sleep(150);
  check('file-type filter narrows to the JPG group, kept whole',
    win.store.getTotalCount() === 1 && win.store.getCount() === 2 &&
    win.store.getAt(0).get('name') === 'IMG_0001.JPG');
  check('rail badge still describes the whole scan (grand)',
    /107|214/.test(String(win.toolsStore.getAt(0).get('badge'))) ||
    win.state.results.duplicates.grand.groups === 107);

  // Keep Newest with the filter active must act on the matches only
  win.autoSelect('newest');
  const selUnderFilter = win.sm.getSelections();
  check('Keep Newest under a search selects within matches only',
    selUnderFilter.length === 1 && /IMG_0001/.test(selUnderFilter[0].get('name')));

  // Select Files in Folder builds its tree from the same filtered store
  {
    const dirs = {};
    win.store.each((r) => { dirs[r.get('path')] = 1; });
    check('folder-dialog universe is the filtered page',
      Object.keys(dirs).length === 2 && win.store.getCount() === 2);
  }

  // custom extension path (server matches case-insensitively, dot tolerated)
  f.type.setValue('ext');
  f.ext.setDisabled(false);
  f.ext.setValue('.mov');
  win.applySearchMenu();
  for (let i = 0; i < 30 && !(win.store.getCount() === 2 && /clip/.test(win.store.getAt(0).get('name'))); i++) await sleep(150);
  check('extension filter finds the MOV pair',
    win.store.getTotalCount() === 1 && /clip\.mov/i.test(win.store.getAt(0).get('name')));

  // location filter: subtree, path-segment safe
  f.type.setValue('');
  f.ext.setValue('');
  win.applySearchMenu();
  for (let i = 0; i < 30 && win.store.getTotalCount() !== 107; i++) await sleep(150);
  // Derive the subtree from a real row — in this harness the daemon's paths
  // carry the fixture tempdir prefix, exactly like the Location combo would
  // (its choices come from state.dirs, the same path space as the rows).
  {
    let manyA = null;
    win.store.each((r) => { if (!manyA && /\/many\/A$/.test(r.get('path'))) manyA = r.get('path'); });
    check('found a many/A row to derive the location from', !!manyA);
    win.state.searchOpts.loc = manyA;
    win.applySearch();
    for (let i = 0; i < 30 && win.store.getTotalCount() !== 105; i++) await sleep(150);
    check('location filter keeps the 105 pg groups (any-member match, whole groups)',
      win.store.getTotalCount() === 105);
  }

  // size filter: nothing in the fixture exceeds 1 MB
  win.state.searchOpts.loc = '';
  win.state.searchOpts.sizeOp = 'gt';
  win.state.searchOpts.sizeMB = 1;
  win.applySearch();
  for (let i = 0; i < 30 && win.store.getTotalCount() !== 0; i++) await sleep(150);
  check('size filter (> 1 MB) matches nothing in the fixture',
    win.store.getTotalCount() === 0);

  // the X wipes options as well as the keyword (File Station behaviour) —
  // otherwise hidden menu filters keep starving an empty-looking search box
  win.searchField.setValue('');
  win.searchField.filter();
  for (let i = 0; i < 30 && win.store.getTotalCount() !== 107; i++) await sleep(150);
  check('the X clears menu options too and restores everything',
    win.store.getTotalCount() === 107 && !win.hasSearchOpts());
  win.sm.clearSelections();

  // Page navigation through the pager itself (the numbered buttons and the
  // arrows all call moveNext/movePrevious/changePage on it).
  const page1First = win.store.getAt(0).get('id');
  win.pager.moveNext();
  for (let i = 0; i < 30 && win.pageUnitCount() === 100; i++) await sleep(150);
  check('next page shows the remaining 7 groups', win.pageUnitCount() === 7);
  check('page 2 rows are different records', win.store.getAt(0).get('id') !== page1First);
  check('whole-set total is unchanged by paging', win.store.getTotalCount() === 107);
  check('summary still describes the whole set on page 2',
    /214 duplicate files<\/b> in 107 groups/.test(win.summaryText.el.dom.innerHTML));
  win.pager.movePrevious();
  for (let i = 0; i < 30 && win.pageUnitCount() !== 100; i++) await sleep(150);
  check('previous page returns to the first 100 groups',
    win.pageUnitCount() === 100 && win.store.getAt(0).get('id') === page1First);
  // Paging replaces the store's records, so a selection does not straddle
  // pages — the same as changing folder in File Station.
  win.sm.selectRow(0, true);
  check('selection possible on the current page', win.sm.getSelections().length === 1);
  win.pager.moveNext();
  for (let i = 0; i < 30 && win.pageUnitCount() === 100; i++) await sleep(150);
  check('changing page clears the selection', win.sm.getSelections().length === 0 &&
    win.moveBtn.disabled);
  win.pager.moveFirst();
  for (let i = 0; i < 30 && win.pageUnitCount() !== 100; i++) await sleep(150);
  check('first-page button returns to page one', win.pageUnitCount() === 100);

  // keep-one-per-group rule: a duplicate group can never be selected whole
  win.sm.clearSelections();
  const g0 = win.store.getAt(0).get('gidx');
  const groupIdx = [];
  win.store.each((r, i) => { if (r.get('gidx') === g0) groupIdx.push(i); });
  groupIdx.forEach((i) => win.sm.selectRow(i, true));
  const selInGroup = win.sm.getSelections().filter((r) => r.get('gidx') === g0).length;
  check('cannot select every file in a group', selInGroup === groupIdx.length - 1);
  // the group's last unselected file gets a disabled checkbox (df-row-capped)
  check('last unselected file in group disables its checkbox',
    doc.querySelectorAll('.df-row-capped').length === 1);
  // The capped row must EXPLAIN itself on the checkbox cell — that hover is
  // the only explanation the capped case has, where the reference case also
  // has the padlock's tooltip.
  {
    const cappedCell = doc.querySelector('.df-row-capped .x-grid3-td-checker');
    check('capped checkbox explains itself on hover',
      !!cappedCell && /at least one file in each group/i.test(cappedCell.getAttribute('ext:qtip') || ''));
    // and trying to select it must NOT fire a toast any more
    const toastsBeforeVeto = window.__toasts || 0;
    const cappedIdx = [...doc.querySelectorAll('.x-grid3-row')].findIndex((r) => r.className.includes('df-row-capped'));
    if (cappedIdx >= 0) win.sm.selectRow(cappedIdx, true);
    check('vetoed selection is silent (no toast)', (window.__toasts || 0) === toastsBeforeVeto);
  }
  win.sm.clearSelections();
  check('disabled state clears with selection', doc.querySelectorAll('.df-row-capped').length === 0);

  // ---- tooltips are TEXT, whatever the daemon's evidence contains ---------
  // Ext QuickTips reads the ext:qtip attribute back (entity-decoded by the
  // browser) and renders it as HTML, so a ZIP member named "<img onerror=…>"
  // inside a scanned file reached the tooltip as markup. The attribute must
  // decode to HTML-safe text: no raw "<" survives one decoding.
  {
    const hostile = '<img src=x onerror=alert(1)>';
    const cm = win.grid.getColumnModel();
    const decodeAttr = (attrText) => {
      const d = doc.createElement('div');
      d.innerHTML = '<span ' + attrText + '></span>';
      return d.firstChild.getAttribute('ext:qtip') || '';
    };
    const evR = cm.getRenderer(cm.findColumnIndex('evidence'));
    const evHtml = evR(hostile, {}, win.store.getAt(0));
    const evDiv = doc.createElement('div');
    evDiv.innerHTML = evHtml;
    const evTip = evDiv.querySelector('span').getAttribute('ext:qtip') || '';
    check('evidence tooltip decodes to text, never markup', evTip.length > 0 && evTip.indexOf('<') < 0 && /&lt;img/.test(evTip));
    check('evidence cell text is escaped', !evDiv.querySelector('img'));
    const meta = {};
    const vR = cm.getRenderer(cm.findColumnIndex('verdict'));
    const fakeRec = { get: (k) => (k === 'evidence' ? hostile : 'corrupt') };
    vR('corrupt', meta, fakeRec);
    const vTip = decodeAttr(meta.attr || '');
    check('status tooltip decodes to text, never markup', vTip.indexOf('<') < 0 && /&lt;img/.test(vTip));
  }

  // ---- a failed page load still ENDS the load ----------------------------
  // Ext's LoadMask hides on the store's load or exception event; a failure
  // reported only through the callback fired neither, and "Loading…" stayed
  // over the grid and the pager for good.
  {
    let exceptions = 0;
    const onExc = () => { exceptions++; };
    win.store.on('exception', onExc);
    const toolBefore = win.state.tool;
    win.state.tool = 'no_such_tool';
    win.state.scanned.no_such_tool = true;
    win.loadPage(0);
    await sleep(600);
    win.store.un('exception', onExc);
    delete win.state.scanned.no_such_tool;
    win.state.tool = toolBefore;
    check('a failed page load fires the store exception (the mask can hide)', exceptions >= 1);
    check('the grid is not left masked after a failed load',
      !win.grid.loadMask || !win.grid.loadMask.el || !win.grid.loadMask.el.isMasked());
    const seq = win.__loads;
    win.loadPage(0);
    await waitForLoadAfter(win, seq, 4);
  }

  // ---- reference-folder protection --------------------------------------
  // Decided by the DAEMON from the folders the stored results were scanned
  // with, on canonical paths — the same comparison the move refuses with.
  // Editing the Scope list therefore changes nothing until the next scan:
  // unlocking rows on the spot would only have the move refuse every one of
  // them as read-only.
  const someDir = win.store.getAt(0).get('path');
  win.state.refDirs.push(someDir);
  win.reloadPage();
  await sleep(600);
  {
    let n = 0;
    win.store.each((r) => { if (r.get('prot')) n++; });
    check('editing the Scope list alone protects nothing until a rescan', n === 0);
  }
  {
    const seq = win.__loads;
    win.startScan();
    for (let i = 0; i < 100; i++) {
      let n = 0;
      if (win.__loads > seq) win.store.each((r) => { if (r.get('prot')) n++; });
      if (n > 0) break;
      await sleep(200);
    }
  }
  let protRows = 0;
  win.store.each((r) => { if (r.get('prot')) protRows++; });
  check('protected rows flagged after scanning with a reference folder', protRows >= 1);
  check('the app records the reference folders the results were scanned with',
    Array.isArray(win.state.scanRefDirs) && win.state.scanRefDirs.indexOf(someDir) >= 0);
  // Protected (read-only reference) rows carry a padlock where the checkbox
  // would be — they are never candidates for selection, so there is no control
  // to disable, and an inert box would misdescribe that. DSM publishes no
  // shared lock class (verified live on DSM 7.4), so the glyph is Unicode
  // U+1F512 rather than artwork.
  check('protected rows flagged with df-row-prot', doc.querySelectorAll('.df-row-prot').length === protRows);
  {
    const protCell = doc.querySelector('.df-row-prot .x-grid3-td-checker');
    check('protected row has a padlock, not a checkbox',
      !!protCell && !!protCell.querySelector('.df-prot-lock') &&
      !protCell.querySelector('.x-grid3-row-checker'));
    // the qtip belongs to the padlock itself, not the cell around it
    const protLock = doc.querySelector('.df-row-prot .df-prot-lock');
    check('padlock is U+1F512, not drawn art',
      !!protLock && protLock.textContent.trim() === '\u{1F512}' &&
      !protLock.querySelector('svg'));
    check('padlock explains itself on hover',
      !!protLock && /read-only reference file/i.test(protLock.getAttribute('ext:qtip') || ''));
    check('protected checkbox cell carries no competing tooltip',
      !!protCell && !protCell.getAttribute('ext:qtip'));
    const protName = doc.querySelector('.df-row-prot .x-grid3-td-name');
    check('protected row explains itself on the name cell',
      !!protName && /read-only reference/i.test(protName.getAttribute('ext:qtip') || protName.innerHTML || ''));
  }
  // Every OTHER row must still carry a stock checker — the renderer override
  // that emits the lock is the one place that could break Ext's string-equality
  // click match for all of them.
  {
    const stock = [...doc.querySelectorAll('.x-grid3-row-checker')];
    check('non-protected rows keep a stock checker after the renderer override',
      stock.length > 0 && stock.every((el) => el.className === 'x-grid3-row-checker'));
    check('no protected row kept a checker', !doc.querySelector('.df-row-prot .x-grid3-row-checker'));
  }
  win.autoSelect('newest');
  let selProt = 0;
  win.sm.getSelections().forEach((r) => { if (r.get('prot')) selProt++; });
  check('bulk select never selects protected rows', win.sm.getSelections().length >= 1 && selProt === 0);
  // Back to no reference folders — by rescanning, which is what applies it.
  win.state.refDirs.pop();
  {
    const seq = win.__loads;
    win.startScan();
    for (let i = 0; i < 100; i++) {
      let n = 0;
      if (win.__loads > seq) win.store.each((r) => { if (r.get('prot')) n++; });
      if (win.__loads > seq && n === 0 && win.store.getCount() > 0) break;
      await sleep(200);
    }
  }
  {
    let n = 0;
    win.store.each((r) => { if (r.get('prot')) n++; });
    check('rescanning without the reference folder clears the padlocks', n === 0 && win.store.getCount() > 0);
  }

  // location links open File Station at the folder (opendir), using a
  // share-space path (not a volume path)
  const volRoot = win.state.volumes[0].path;
  win.openFileStation(volRoot + '/Backups/A');
  check('File Station opened via opendir with share-space path',
    window.__fsLaunchCfg && window.__fsLaunchCfg.opendir === '/Backups/A');

  // tool switch → idle card for a tool that has not been scanned yet
  win.setTool('temp_files');
  check('unscanned tool shows the idle card', win.centerPanel.getLayout().activeItem === win.idlePanel);
  check('match button hidden off duplicates', win.matchBtn.hidden);
  // empty-folder scan: truly empty dirs are listed; unreadable dirs (hidden
  // contents, like a Hyper Backup vault without permissions) are NOT
  win.setTool('empty_folders');
  const efSeq = win.__loads;
  win.startScan();
  for (let i = 0; i < 40 && !win.state.scanned.empty_folders; i++) await sleep(300);
  check('empty-folder scan completed', !!win.state.scanned.empty_folders);
  await waitForLoadAfter(win, efSeq, 1);
  const efNames = [];
  win.store.each((r) => efNames.push(r.get('name')));
  check('truly empty folder listed', efNames.indexOf('emptydir') >= 0);

  // Rows File Station cannot address get DSM's DISABLED checkbox — the same
  // sprite the last-copy case uses — and cannot be selected at all. The
  // fixture's .DS_Store is the real thing: the walk finds it, File Station
  // cannot see it, so no move could ever succeed.
  {
    const tfSeq = win.__loads;
    win.setTool('temp_files');
    win.startScan();
    for (let i = 0; i < 40 && !win.state.scanned.temp_files; i++) await sleep(300);
    await waitForLoadAfter(win, tfSeq, 1);
    let dsRec = null, plainRec = null;
    win.store.each((r) => {
      if (r.get('name') === '.DS_Store') dsRec = r;
      else if (!r.get('nomove') && !plainRec) plainRec = r;
    });
    check('the .DS_Store row reached the grid', !!dsRec);
    check('daemon marked it nomove', !!dsRec && dsRec.get('nomove') === true);
    check('an ordinary temp row is not marked nomove', !!plainRec && !plainRec.get('nomove'));
    const gv = win.grid.getView();
    check('unmovable row carries df-row-nomove',
      !!dsRec && / df-row-nomove|^df-row-nomove/.test(gv.getRowClass(dsRec, 0, {}, win.store) || ''));
    check('a movable row does not',
      !!plainRec && !/df-row-nomove/.test(gv.getRowClass(plainRec, 0, {}, win.store) || ''));
    // The sprite comes from a ROW-scoped rule, never a second class on the
    // checker: Ext matches the checker's className by ==, so an extra class
    // there kills every checkbox click in the grid.
    check('nomove sprite is row-scoped, not on the checker itself',
      /df-row-nomove \.x-grid3-row-checker\{background-position:0 -40px/.test(cssText));
    const stillStock = [...doc.querySelectorAll('.x-grid3-row-checker')];
    check('checker className stays exactly stock with an unmovable row on screen',
      stillStock.length > 0 && stillStock.every((el) => el.className === 'x-grid3-row-checker'));
    check('selection is refused for an unmovable row',
      !!dsRec && win.sm.selectRecords([dsRec]) === undefined && !win.sm.isSelected(dsRec));
    // The daemon's own refusal is asserted in the raw-API section below,
    // where the api() helper is in scope.
  }
  // Leave the UI on the tool the following assertions expect.
  win.setTool('empty_folders');
  await sleep(50);
  check('unreadable folder not reported as empty', efNames.indexOf('locked') < 0);
  // junk-only counts as empty: the junk-file names are
  // the Temporary Files tool's own list, and @eaDir is Synology's regenerable
  // thumbnail cache. A dot-DIRECTORY with real content stays protected — a
  // folder whose only child is .git is a repository, not clutter.
  check('folder with only a hidden content directory not reported empty', efNames.indexOf('hiddenonly') < 0);
  check('folder with only @eaDir reported empty (junk)', efNames.indexOf('eadironly') >= 0);
  check('folder with only Thumbs.db reported empty (junk)', efNames.indexOf('junkonly') >= 0);
  check('folder with only .DS_Store reported empty (junk)', efNames.indexOf('dsonly') >= 0);
  check('junk beside a real file does not make a folder empty', efNames.indexOf('junkmixed') < 0);

  // flat tools sort server-side: a header click funnels through store.sort,
  // which must re-request the loaded window in the new order
  win.setTool('empty_files');
  const emSeq = win.__loads;
  win.startScan();
  for (let i = 0; i < 40 && !win.state.scanned.empty_files; i++) await sleep(300);
  check('empty-files scan completed', !!win.state.scanned.empty_files);
  await waitForLoadAfter(win, emSeq, 2);
  check('empty files listed', win.store.getCount() >= 2);
  check('flat tool pages in rows and counts items',
    win.pager.pageSize === 1000 && win.pager.displayMsg === '{2} items');
  const efFirstBefore = win.store.getAt(0).get('name');
  win.store.sort('name', 'DESC');
  for (let i = 0; i < 30; i++) {
    if (win.store.getCount() > 0 && win.store.getAt(0).get('name') !== efFirstBefore) break;
    await sleep(150);
  }
  const efp = win.fetchParams('empty_files');
  check('flat sort re-requested from the daemon (name DESC)', efp.sort === 'name' && efp.dir === 'DESC');
  check('rows arrive in the requested order', win.store.getCount() >= 2 &&
    win.store.getAt(0).get('name') >= win.store.getAt(win.store.getCount() - 1).get('name'));

  // ---- Conflicting Files -----------------------------------------------
  // Filled in by the duplicates scan that ran at the top of this suite, so
  // there is nothing to scan here: reaching it already exercises the
  // completion poll refreshing a second tool. The fixture holds two sets —
  // Backups/damaged/archive.dat (one copy zero-filled past the halfway mark,
  // which is decidable) and notes-a/notes-b (same size and mtime, different
  // names, no evidence either way).
  check('duplicates scan also populated corrupted files',
    !!win.state.scanned.corrupted_files);
  win.setTool('corrupted_files');
  const cfSeq = win.__loads;
  await waitForLoadAfter(win, cfSeq, 1);
  check('corrupted files render as a grouped listing',
    win.store.groupField === 'gidx' && /claim to be the same file/.test(win.store.getAt(0).get('grpLabel')));
  check('pager counts sets, not items',
    win.pager.pageSize === 100 && win.pager.displayMsg === '{2} sets');

  const cfRows = {};
  win.store.each((r) => { cfRows[r.get('path') + '/' + r.get('name')] = r; });
  const dmg = Object.keys(cfRows).filter((k) => /archive\.dat$/.test(k));
  check('both copies of the damaged file are listed', dmg.length === 2);
  const truncated = cfRows[Object.keys(cfRows).find((k) => /damaged\/copy\/archive\.dat$/.test(k))];
  const whole = cfRows[Object.keys(cfRows).find((k) => /damaged\/archive\.dat$/.test(k))];
  check('the zero-filled copy is identified as corrupted',
    !!truncated && truncated.get('verdict') === 'corrupt');
  check('its twin is identified as intact',
    !!whole && whole.get('verdict') === 'intact');
  check('the verdict comes with the evidence for it (' +
    JSON.stringify((truncated && truncated.get('evidence') || '').slice(0, 60)) + ')',
    !!truncated && /zero/i.test(truncated.get('evidence') || ''));
  const notes = Object.keys(cfRows).filter((k) => /notes-[ab]\.txt$/.test(k)).map((k) => cfRows[k]);
  check('a set with no evidence either way lists both copies', notes.length === 2);
  check('and convicts neither of them',
    notes.every((r) => r.get('verdict') === 'unknown'));

  // The Status column and the CSV export name the same three verdicts from two
  // different codebases, so only a test keeps them saying the same word. They
  // drifted once — the export wrote "Unknown" against the grid's
  // "Undetermined" — so a report disagreed with the screen it came from. The
  // matching half is TestVerdictLabelsMatchTheUI in server/corrupt_test.go.
  const verdictLabels = Array.from(doc.querySelectorAll('.df-verdict')).map((e) => e.textContent.trim());
  const labelCount = (w) => verdictLabels.filter((t) => t === w).length;
  check('Status renders the exact words the CSV export writes (' + JSON.stringify(verdictLabels) + ')',
    verdictLabels.length === 4 && labelCount('Corrupted') === 1 &&
    labelCount('Intact') === 1 && labelCount('Undetermined') === 2);

  // Read-only: the report offers no action, and the veto is on the one
  // chokepoint every selection path goes through.
  const cfCm = win.grid.getColumnModel();
  check('Status column is visible on this tool', !cfCm.isHidden(cfCm.findColumnIndex('verdict')));
  check('Evidence column is visible on this tool', !cfCm.isHidden(cfCm.findColumnIndex('evidence')));
  check('the checkbox column is hidden where nothing can be selected', cfCm.isHidden(0));
  check('Move is hidden on a read-only tool', win.moveBtn.hidden === true);
  check('the Select menu is hidden too', win.selectMenuBtn.hidden === true);
  win.sm.selectRow(0, true);
  check('rows cannot be selected at all', win.sm.getSelections().length === 0);
  check('the footer says what to do instead of offering an action',
    /File Station/.test(win.selText.el ? win.selText.el.dom.innerHTML : ''));
  const cfSummary = win.summaryText.el.dom.innerHTML;
  check('summary leads with what was actually identified (' + cfSummary.slice(0, 90) + ')',
    /identified as damaged/.test(cfSummary));
  // The Scan button on this tool runs the scan that fills it in.
  check('its Scan button runs the duplicates scan',
    win.fetchParams('corrupted_files').match === undefined);

  win.setTool('duplicates');
  await sleep(200);
  check('Status column goes away on the other tools',
    cfCm.isHidden(cfCm.findColumnIndex('verdict')) && !cfCm.isHidden(0));
  check('back to duplicates grid', win.centerPanel.getLayout().activeItem === win.grid);

  // export flow uses DSM's FileChooser (SDK). Verify it's invoked with the
  // folder-select usage, then simulate a choice and confirm export fires.
  window.__chooserOpened = 0;
  win.openExportDialog();
  await sleep(200);
  check('export opens the DSM FileChooser', window.__chooserOpened === 1);
  const chooser = window.__lastChooser;
  check('chooser requested folder mode', !!chooser && chooser.chooserConfig.usage.type === 'chooseDir');
  let exportErr = null;
  win.notify = function (m) { exportErr = m; };
  // simulate the user picking a share-space folder that maps to a real volume
  chooser.fireEvent('choose', chooser, { path: '/Backups' });
  await sleep(600);
  check('share path mapped and export succeeded (' + JSON.stringify(exportErr) + ')', /exported/i.test(exportErr || ''));

  // select-in-folder dialog: counts are plain text (no raw HTML)
  win.openDirSelectDialog();
  await sleep(400);
  const dirTreeText = doc.body.textContent;
  check('dir-select shows plain-text counts', /\(\d+ files?\)/.test(dirTreeText));
  check('dir-select has no raw HTML in labels', dirTreeText.indexOf('<span') < 0);
  doc.querySelectorAll('button').forEach((b) => {
    if (/^Cancel$/.test(b.textContent.trim())) b.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  });
  await sleep(200);

  // match option subdivision — applied by the daemon (the grid holds one
  // page); the fixture pairs share basenames, so every group survives
  // match-by-name, and the refinement re-pages from the first page
  win.sm.clearSelections();
  win.onMatchToggled('name', true, 'File Name');
  for (let i = 0; i < 30 && !win.store.getCount(); i++) await sleep(150);
  check('match-by-name refetch keeps groups valid',
    win.fetchParams('duplicates').match.name === true && win.store.getCount() >= 4 &&
    win.store.getTotalCount() === 107);
  win.onMatchToggled('name', false, 'File Name');
  for (let i = 0; i < 30 && win.fetchParams('duplicates').match.name; i++) await sleep(150);

  // ---- UI move flow: busy dialog, long-op timeout, in-place refresh ------
  // Moving one file of a pg pair (IMG/clip pairs stay intact for the raw
  // keep-one tests below). The move request must carry the effectively
  // unlimited timeout, mask the app behind a busy dialog while File Station
  // works, and refresh the loaded window in place afterwards.
  const fsm = require('fs');
  const uiDest = volRoot + '/moved-ui';
  fsm.mkdirSync(uiDest, { recursive: true });
  win.sm.clearSelections();
  const moveRec = win.store.getAt(4); // first pg-pair group starts at row 4
  const movedName = moveRec.get('name');
  win.sm.selectRow(4, true);
  let moveMsg = null;
  win.notify = function (m) { moveMsg = m; };
  let moveTimeout = null;
  const origAjax = window.Ext.Ajax.request;
  window.Ext.Ajax.request = function (o) { moveTimeout = o.timeout; return origAjax.apply(this, arguments); };
  win.doMove(win.sm.getSelections(), uiDest, false);
  window.Ext.Ajax.request = origAjax;
  check('move request carries the long-op timeout', moveTimeout === 12 * 3600000);
  check('busy dialog masks the app during the move', /Moving 1 file/.test(doc.body.textContent));
  for (let i = 0; i < 40 && !/Moved 1 file/.test(moveMsg || ''); i++) await sleep(200);
  check('move completed and reported (' + JSON.stringify(moveMsg) + ')', /Moved 1 file/.test(moveMsg || ''));
  // the page reload that follows the move is a round trip of its own
  const stillListed = () => {
    let seen = false;
    win.store.each((r) => { if (r.get('name') === movedName) seen = true; });
    return seen;
  };
  for (let i = 0; i < 40 && stillListed(); i++) await sleep(200);
  check('dissolved group left the reloaded page', !stillListed());
  check('busy dialog closed after the move', !/Moving 1 file/.test(doc.body.textContent));
  check('moved file landed at the destination', fsm.existsSync(uiDest + '/' + movedName));

  // ---- UI move flow, dialog-driven: the option comes BEFORE the picker ----
  // The block above calls doMove directly, so it proves nothing about the
  // wiring. This one drives the real flow with synthetic clicks on rendered
  // components: Move opens the preserve-option dialog FIRST, and the folder
  // picker's Select executes the move — there is no confirmation after it.
  check('the post-pick confirmation dialog is gone', typeof win.confirmMove !== 'function');
  // The destination must sit under a share that existed at bootstrap: VOLUMES
  // is a one-shot snapshot, so shareToReal cannot map a top-level folder
  // created mid-run. '/spare' is a real fixture share.
  const dlgDest = volRoot + '/spare/moved-ui2';
  fsm.mkdirSync(dlgDest, { recursive: true });
  const btns = (re) => Array.from(doc.querySelectorAll('button'))
    .filter((b) => re.test(b.textContent.trim()));
  const clickBtn = (re) => {
    const hits = btns(re);
    hits.forEach((b) => b.dispatchEvent(new window.MouseEvent('click', { bubbles: true })));
    return hits.length;
  };

  // pass 1 — preserve UNTICKED: the files must land flat
  win.sm.clearSelections();
  for (let i = 0; i < 40 && !win.store.getCount(); i++) await sleep(200);
  const rec2 = win.store.getAt(4);
  const name2 = rec2.get('name');
  win.sm.selectRow(4, true);
  window.__chooserOpened = 0;
  window.__chooserClosed = 0;
  win.openMoveDialog();
  await sleep(200);
  check('options dialog opens BEFORE any folder picker', window.__chooserOpened === 0);
  // Same DSM contract as the progress dialog: owner is what stops the app
  // window burying it. This one matters most — the move it starts is
  // irreversible and there is no confirmation after the folder pick.
  check('options dialog is owned by the app window', win._moveWin.owner === win);
  // The two destination layouts are offered as mutually exclusive radios, so
  // the alternative is visible without reading any prose.
  check('options dialog offers the preserve choice',
    /Preserve original folder structure/.test(doc.body.textContent));
  check('options dialog offers the flat alternative',
    /Move all files into same folder/.test(doc.body.textContent));
  check('options dialog states nothing is overwritten',
    /No files will be overwritten/.test(doc.body.textContent));
  check('no post-pick confirm button exists', btns(/^Move Files$/).length === 0);
  // Locate each radio by its boxLabel's <label for=…> — the grid and the
  // search menu render inputs too.
  const radioFor = (re) => {
    const l = Array.from(doc.querySelectorAll('label')).find((x) => re.test(x.textContent));
    return l ? doc.getElementById(l.htmlFor) : null;
  };
  const presRadio = radioFor(/Preserve original folder structure/);
  const flatRadio = radioFor(/Move all files into same folder/);
  check('both destination modes are rendered as radios',
    !!presRadio && !!flatRadio &&
    presRadio.type === 'radio' && flatRadio.type === 'radio');
  check('preserve is the default selection', presRadio.checked === true && flatRadio.checked === false);
  // Move-time verification is the user's call, asked here, defaulting to the
  // safe side, and the dialog never decides by itself.
  const verifyInput = radioFor(/Verify file contents after moving/);
  check('verification is offered as a checkbox, ticked by default',
    !!verifyInput && verifyInput.type === 'checkbox' && verifyInput.checked === true);
  // The prose explaining WHEN turning verification off is reasonable (a move
  // within one shared folder only renames) is deliberately kept out of the
  // dialog and lives in the README alone — the checkbox is the whole
  // question, and the paragraph would be three lines of the dialog's height.
  check('the dialog carries no same-shared-folder explanation',
    !/only renames the file/.test(doc.body.textContent));
  // Untick for pass 1: the payload must carry the explicit opt-out.
  verifyInput.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  check('the verification checkbox can be turned off', verifyInput.checked === false);
  // Selecting the flat option must actually deselect preserve — a radio pair
  // that does not group would leave both set and silently preserve anyway.
  flatRadio.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  check('choosing the flat option clears the preserve one',
    flatRadio.checked === true && presRadio.checked === false);
  check('the primary button is Choose Folder', clickBtn(/^Choose Folder/) === 1);
  await sleep(200);
  check('the primary button opens the DSM FileChooser', window.__chooserOpened === 1);
  const mc = window.__lastChooser;
  check('move chooser requested folder mode',
    !!mc && mc.chooserConfig.usage.type === 'chooseDir');
  // The request now fires from inside the chooser's callback, so the spy has
  // to be installed around the 'choose' event, not around openMoveDialog.
  let msg2 = null, body2 = null, tmo2 = null;
  win.notify = function (m) { msg2 = m; };
  const oa2 = window.Ext.Ajax.request;
  window.Ext.Ajax.request = function (o) {
    if (/\/move(\?|$)/.test(o.url || '')) { body2 = o.jsonData; tmo2 = o.timeout; }
    return oa2.apply(this, arguments);
  };
  mc.fireEvent('choose', mc, { path: '/spare/moved-ui2' });
  window.Ext.Ajax.request = oa2;
  check('the move request actually fired from the picker', body2 !== null);
  check("the picker's Select starts the move immediately",
    /Moving 1 file/.test(doc.body.textContent));
  check('the chooser closed before the move started', window.__chooserClosed === 1);
  check('dialog-driven move keeps the long-op timeout', tmo2 === 12 * 3600000);
  check('payload carries the flat choice and the tool id',
    body2.preserve === false && body2.tool === 'duplicates');
  check('unticking verification is sent as an explicit false',
    body2.verify === false);
  for (let i = 0; i < 40 && !/Moved 1 file/.test(msg2 || ''); i++) await sleep(200);
  check('the flat choice landed the file flat (' + JSON.stringify(msg2) + ')',
    fsm.existsSync(dlgDest + '/' + name2));

  // pass 2 — preserve LEFT TICKED (the default): one "Duplicates" folder,
  // with the source's own folder chain mirrored inside it
  win.sm.clearSelections();
  for (let i = 0; i < 40 && !win.store.getCount(); i++) await sleep(200);
  const rec3 = win.store.getAt(4);
  const name3 = rec3.get('name'), dir3 = rec3.get('path');
  win.sm.selectRow(4, true);
  window.__chooserOpened = 0;
  msg2 = null; body2 = null;
  win.openMoveDialog();
  await sleep(200);
  check('a second flow can be started after the first settled', window.__chooserOpened === 0);
  clickBtn(/^Choose Folder/);
  await sleep(200);
  window.Ext.Ajax.request = function (o) {
    if (/\/move(\?|$)/.test(o.url || '')) body2 = o.jsonData;
    return oa2.apply(this, arguments);
  };
  window.__lastChooser.fireEvent('choose', window.__lastChooser, { path: '/spare/moved-ui2' });
  window.Ext.Ajax.request = oa2;
  check('preserve is sent when the default radio is left alone',
    !!body2 && body2.preserve === true);
  check('verification is sent ON when the checkbox is left alone',
    !!body2 && body2.verify === true);
  for (let i = 0; i < 40 && !/Moved 1 file/.test(msg2 || ''); i++) await sleep(200);
  check('preserve-mode dialog move lands under the tool folder (' + JSON.stringify(msg2) + ')',
    fsm.existsSync(dlgDest + '/Duplicates' + dir3 + '/' + name3));
  check('the toast names the folder that was created', /Duplicates/.test(msg2 || ''));

  // ---- external (USB-shaped) share as destination ------------------------
  // The fixture's second volume reproduces a real USB disk's defining trap:
  // the share NAME ("usbshare1") differs from the directory it lives in
  // (…/volumeUSB1/usbshare). Every mapping between share space and volume
  // space must go through the {name, path} pairs from /api/info — the old
  // name-concatenation broke on exactly this and answered every external
  // pick with "This location cannot be used".
  const usbVol = win.state.volumes.filter((v) => /USB/i.test(v.name || ''))[0];
  check('the USB-shaped volume is reported by /info', !!usbVol);
  const usbShare = usbVol && (usbVol.shares || [])[0];
  check('its share name differs from its directory',
    !!usbShare && usbShare.name === 'usbshare1' && /\/usbshare$/.test(usbShare.path));
  const usbReal = usbShare.path;

  // The picker hands back share-space paths; the pick must resolve — this
  // exact call answered null (→ "This location cannot be used") before.
  win.sm.clearSelections();
  for (let i = 0; i < 40 && !win.store.getCount(); i++) await sleep(200);
  const recU = win.store.getAt(4);
  const nameU = recU.get('name'), dirU = recU.get('path');
  win.sm.selectRow(4, true);
  msg2 = null; body2 = null;
  win.openMoveDialog();
  await sleep(200);
  clickBtn(/^Choose Folder/);
  await sleep(200);
  window.Ext.Ajax.request = function (o) {
    if (/\/move(\?|$)/.test(o.url || '')) body2 = o.jsonData;
    return oa2.apply(this, arguments);
  };
  window.__lastChooser.fireEvent('choose', window.__lastChooser, { path: '/usbshare1' });
  window.Ext.Ajax.request = oa2;
  check('picking the USB share resolves instead of "cannot be used"',
    !!body2 && body2.dest === usbReal);
  for (let i = 0; i < 40 && !/Moved 1 file/.test(msg2 || ''); i++) await sleep(200);
  check('the moved file landed inside the usb directory (' + JSON.stringify(msg2) + ')',
    fsm.existsSync(usbReal + '/Duplicates' + dirU + '/' + nameU));
  check('nothing landed at a concatenated wrong path',
    !fsm.existsSync(usbVol.path + '/usbshare1'));

  // ---- busy dialog shows overall progress --------------------------------
  // /move is one blocking request, so the bar is driven by polling /state.
  // Drive the real showBusy the way doMove's poller does and assert the bar
  // becomes determinate and shows the count — an indeterminate bar that never
  // switches is exactly what this replaced.
  const busyWin = win.showBusy('Moving 12 files…', 'testing progress');
  await sleep(100);
  const moveBarEl = () => doc.querySelector('.x-progress-bar');
  // The count line lives OUTSIDE the bar. Reading the whole dialog body is
  // deliberate: it would also catch the text reappearing inside the bar.
  const moveBarText = () => busyWin.body.dom.textContent.replace(/\s+/g, ' ').trim();
  const inBarText = () => {
    const t = doc.querySelector('.x-progress-text');
    return t ? t.textContent.trim() : '';
  };
  check('busy dialog renders a progress bar', !!moveBarEl());
  check('busy dialog exposes setProgress for the poller', typeof busyWin.setProgress === 'function');
  const pbar0 = busyWin.findByType('progress')[0];
  check('the bar starts indeterminate while nothing is known', !!pbar0.waitTimer);
  busyWin.setProgress(3, 12, '3 of 12 — clip.mov');
  await sleep(50);
  // wait() runs a repeating animation that rewrites the bar every 200ms. If it
  // is not stopped it fights every updateProgress, and the bar jitters between
  // the real fraction and the sweep — invisible to a value assertion, obvious
  // on screen.
  check('the indeterminate animation stops once real progress arrives', !pbar0.waitTimer);
  check('progress bar reports the overall count', /3 of 12/.test(moveBarText()));
  check('progress bar names the file in flight', /clip\.mov/.test(moveBarText()));
  // Ext renders progress text INSIDE the bar element, where an 18px-high bar
  // clips it to a sliver. The count must live in its own line, as the scan
  // dialog's does.
  check('the count is not rendered inside the bar, where it would be clipped',
    inBarText() === '');
  // jsdom does no layout, so the bar's rendered WIDTH is 0px whatever the
  // value — assert the component's own fraction instead.
  const pbar = busyWin.findByType('progress')[0];
  const v3 = pbar.value;
  busyWin.setProgress(9, 12, '9 of 12 — later.mov');
  await sleep(50);
  const v9 = pbar.value;
  check('progress bar advances with the count (' + v3 + ' -> ' + v9 + ')',
    Math.abs(v3 - 0.25) < 0.001 && Math.abs(v9 - 0.75) < 0.001);
  // A total of 0 would divide by zero and blank the bar; it must be ignored.
  busyWin.setProgress(0, 0, 'nonsense');
  await sleep(50);
  check('a zero total is ignored rather than blanking the bar', /9 of 12/.test(moveBarText()));
  busyWin.close();
  await sleep(100);
  check('busy dialog closes', !/testing progress/.test(doc.body.textContent));

  // The move's busy dialog carries NO explanatory paragraph — the title and
  // the count line say it all. showBusy still renders one when a caller passes
  // text (export does), so the omission has to be the caller's, not a lost
  // feature.
  const bareWin = win.showBusy('Moving 5 files…');
  await sleep(100);
  const bareText = bareWin.body.dom.textContent.replace(/\s+/g, ' ').trim();
  check('a busy dialog with no description renders none', bareText === '');
  check('it still has its progress bar', !!bareWin.findByType('progress')[0]);
  bareWin.setProgress(2, 5, '2 of 5 — x.bin');
  await sleep(50);
  check('the bare dialog still reports progress',
    /2 of 5 — x\.bin/.test(bareWin.body.dom.textContent));
  bareWin.close();
  await sleep(50);
  const descWin = win.showBusy('Exporting…', 'a description');
  await sleep(100);
  check('a busy dialog given text still shows it',
    /a description/.test(descWin.body.dom.textContent));
  descWin.close();
  await sleep(50);

  // A second options dialog must not stack while the first is open.
  win.sm.clearSelections();
  for (let i = 0; i < 40 && !win.store.getCount(); i++) await sleep(200);
  win.sm.selectRow(4, true);
  // Counted from HERE, not from a reset several blocks earlier: this asserts
  // "cancelling opens no chooser", and against a running total it instead
  // asserts "no earlier test opened one" — which is how adding the USB case
  // above broke it. Scope the counter to the thing being measured.
  window.__chooserOpened = 0;
  win.openMoveDialog();
  await sleep(150);
  win.openMoveDialog();
  await sleep(150);
  check('a second Move click does not stack a second options dialog',
    btns(/^Choose Folder/).length === 1);
  clickBtn(/^Cancel$/);
  await sleep(200);
  check('cancelling the options dialog opens no chooser and moves nothing',
    window.__chooserOpened === 0);

  // Two folder choosers can be open at once by design (the chooser has no
  // cancel hook, so it is deliberately not flag-guarded). The second Select
  // is caught here instead — and this guard is what stops it reaching the
  // daemon, which would refuse it with a message about scan results that
  // explains nothing.
  win.sm.selectRow(4, true);
  let msg3 = null, fired3 = null;
  win.notify = function (m) { msg3 = m; };
  window.Ext.Ajax.request = function (o) {
    if (/\/move(\?|$)/.test(o.url || '')) fired3 = o.jsonData;
    return oa2.apply(this, arguments);
  };
  win._moveInFlight = true;
  win.doMove(win.sm.getSelections(), dlgDest, false, 'duplicates');
  window.Ext.Ajax.request = oa2;
  win._moveInFlight = false;
  check('a move started while one is running is refused, not sent',
    fired3 === null && /already running/.test(msg3 || ''));
  win.sm.clearSelections();

  // A selection REPLACED between opening the dialog and picking the folder
  // must not move the originally captured batch — the pick IS the move, with
  // no confirmation after it.
  //
  // Note what this test does and does not prove. On real DSM 7.4 the chooser
  // is an owned SYNO.SDS.ModalWindow, so it masks the app window and a USER
  // cannot click the grid mid-flow (measured on DSM 7.4: elementFromPoint
  // inside the app window returns .ext-el-mask.sds-window-mask). The harness
  // has no such mask — harness.html stubs ModalWindow as a plain Ext.Window —
  // so the swap below is performed programmatically. It pins the guard, not
  // a reachable user journey: this is defence in depth against DSM's masking
  // changing under us, and against any programmatic path that mutates the
  // selection while a pick is outstanding.
  check('enough rows to swap the selection', win.store.getCount() > 5);
  win.sm.selectRow(4, true);
  const swapFrom = win.sm.getSelections()[0].id;
  window.__chooserOpened = 0;
  win.openMoveDialog();
  await sleep(150);
  clickBtn(/^Choose Folder/);
  await sleep(200);
  check('swap case: the chooser is open with a batch captured',
    window.__chooserOpened === 1);
  // the user now picks a DIFFERENT row while the chooser is still up
  win.sm.clearSelections();
  win.sm.selectRow(5, true);
  const swapTo = win.sm.getSelections()[0].id;
  check('the swap is a real change, not the same row twice', swapFrom !== swapTo);
  let msg4 = null, fired4 = null;
  win.notify = function (m) { msg4 = m; };
  window.Ext.Ajax.request = function (o) {
    if (/\/move(\?|$)/.test(o.url || '')) fired4 = o.jsonData;
    return oa2.apply(this, arguments);
  };
  const swapChooser = window.__lastChooser;
  swapChooser.fireEvent('choose', swapChooser, { path: '/spare/moved-ui2' });
  await sleep(300);
  window.Ext.Ajax.request = oa2;
  check('swapping the selection while the chooser is open moves nothing',
    fired4 === null && /selection changed/i.test(msg4 || ''));
  win.sm.clearSelections();

  // ---- interrupted-scan choice: resume or start over ---------------------
  // An interrupted duplicates scan must give the user the choice at the
  // moment they initiate the next one; resume means CONTINUING that run, and
  // the flag rides the /scan payload for the daemon's marker-gated check.
  // startScan revalidates against /state before showing the dialog, so the
  // stub answers /state too — with or without the notice, per leg.
  {
    let scanBody = null;
    // The daemon's marker records the interrupted run's whole request, and
    // Resume is only live when it matches the current settings — so the
    // planted notices mirror the harness state exactly, and the drift leg
    // below varies one field.
    const noticeFor = () => ({ tool: 'duplicates', gen: 3,
      dirs: win.state.dirs.slice(), refDirs: (win.state.refDirs || []).slice(),
      recurse: true,
      match: { name: !!win.state.match.name, modified: !!win.state.match.modified,
        created: !!win.state.match.created } });
    const resumeDisabled = () => {
      const b = btns(/^Resume$/)[0];
      return !!(b && b.closest('.x-item-disabled'));
    };
    let stateNotice = noticeFor();
    const oaScan = window.Ext.Ajax.request;
    window.Ext.Ajax.request = function (o) {
      if (/\/scan(\?|$)/.test(o.url || '')) { scanBody = o.jsonData; return; }
      if (/\/state(\?|$)/.test(o.url || '')) {
        const body = stateNotice ? { interrupted: stateNotice } : {};
        o.callback.call(o.scope || null, o, true,
          { status: 200, responseText: JSON.stringify(body) });
        return;
      }
      return oaScan.apply(this, arguments);
    };
    win._interrupted = noticeFor();
    win.startScan();
    await sleep(100);
    check('an interrupted scan opens the choice dialog instead of scanning',
      scanBody === null && /Interrupted Scan/.test(doc.body.textContent));
    check('the dialog offers Resume, Start Over and Cancel',
      btns(/^Resume$/).length === 1 && btns(/^Start Over$/).length === 1);
    check('the dialog says what resuming means',
      /files it already compared are not read again/.test(doc.body.textContent));
    check('Resume is live while the settings still match the interrupted run',
      !resumeDisabled());
    clickBtn(/^Resume$/);
    await sleep(100);
    check('Resume sends resume:true', !!scanBody && scanBody.resume === true);
    // The stub swallowed the request — the daemon never accepted a scan — so
    // the offer must still stand: only an ACCEPTED scan clears it. A refusal
    // ("a move is running") between click and admission must not cost the
    // user their resumable run.
    check('the offer survives an unaccepted scan request', win._interrupted !== null);
    // Start Over: same dialog, explicit false.
    scanBody = null;
    win.startScan();
    await sleep(100);
    clickBtn(/^Start Over$/);
    await sleep(100);
    check('Start Over sends resume:false', !!scanBody && scanBody.resume === false);
    // Settings drift: the daemon honors a resume only for the identical
    // request (scanMarker.matches), so a notice recorded under a different
    // scope opens the dialog with Resume DISABLED and the reason shown —
    // never a live button whose click the daemon would quietly turn into a
    // full re-read.
    scanBody = null;
    stateNotice = Object.assign(noticeFor(), { dirs: ['/volume1/somewhere-else'] });
    win._interrupted = noticeFor();
    win.startScan();
    await sleep(100);
    check('a drifted notice still opens the dialog, not a scan',
      scanBody === null && /Interrupted Scan/.test(doc.body.textContent));
    check('and disables Resume with the reason shown',
      resumeDisabled() && /Resume is unavailable: the scan settings/.test(doc.body.textContent));
    clickBtn(/^Resume$/);
    await sleep(100);
    check('a click on the disabled Resume moves nothing', scanBody === null);
    clickBtn(/^Start Over$/);
    await sleep(100);
    check('Start Over remains live under drifted settings',
      !!scanBody && scanBody.resume === false);
    // Stale stash: the daemon's notice is gone (another session scanned) —
    // no dialog may promise a resume that will not happen; the scan starts
    // directly and the stash clears.
    scanBody = null;
    stateNotice = null;
    win._interrupted = noticeFor();
    win.startScan();
    await sleep(100);
    check('a stale offer is dropped after revalidation, scanning directly',
      !!scanBody && scanBody.resume === false &&
      !/Interrupted Scan/.test(doc.body.textContent) && win._interrupted === null);
    // No stash at all: no /state round trip needed, straight to the scan.
    scanBody = null;
    win.startScan();
    await sleep(100);
    check('without an interruption the scan starts directly with resume:false',
      !!scanBody && scanBody.resume === false);
    // pollStatus learns about an interruption that happens while the app is
    // OPEN (daemon died mid-scan and came back): it must re-arm the offer
    // and report an interruption, never a completion.
    stateNotice = Object.assign(noticeFor(), { gen: 5 });
    const oldNotify = win.notify;
    let toastMsg = null;
    win.notify = function (m) { toastMsg = m; };
    win.pollStatus();
    await sleep(100);
    win.notify = oldNotify;
    check('a mid-session interruption re-arms the resume offer',
      !!win._interrupted && win._interrupted.gen === 5);
    check('and is announced as an interruption, not a completion',
      /interrupted by a restart/.test(toastMsg || ''));
    // A Scan on a DIFFERENT tool clears the daemon's notice — and the dead
    // run's generation with it — so it must ask first, never discard
    // silently behind the toast's promise of a resume offer.
    stateNotice = noticeFor();
    win._interrupted = noticeFor();
    scanBody = null;
    const toolBefore = win.state.tool;
    win.state.tool = 'empty_folders';
    win.startScan();
    await sleep(100);
    check('a Scan on another tool asks before discarding the interrupted run',
      scanBody === null && /Discard the interrupted scan\?/.test(doc.body.textContent));
    clickBtn(/^Cancel$/);
    await sleep(100);
    check('Cancel keeps the resumable run and starts nothing',
      scanBody === null && !!win._interrupted && !/Discard the interrupted scan\?/.test(doc.body.textContent));
    win.startScan();
    await sleep(100);
    clickBtn(/^Scan Anyway$/);
    await sleep(100);
    check("Scan Anyway runs the other tool's scan", !!scanBody && scanBody.tool === 'empty_folders');
    win.state.tool = toolBefore;
    win._interrupted = null;
    window.Ext.Ajax.request = oaScan;
  }

  // ---- raw daemon API: the server is the trust boundary ------------------
  // The UI enforces these invariants too, but any local client can POST to
  // the daemon directly, so the daemon must enforce them itself.
  const API = 'http://localhost:' + PORT + '/api';
  const api = async (p, body) => {
    const res = await fetch(API + p, body === undefined ? {} : {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    return res.json();
  };
  // The read-only rule belongs to the daemon, not the UI: hiding the Move
  // button is presentation. A raw caller must be refused both ways, or
  // "Conflicting Files never moves anything" is only true of the buttons.
  const roScan = await api('/scan', { tool: 'corrupted_files', dirs: [volRoot], recurse: true });
  check('daemon refuses a direct scan of the read-only tool (' + JSON.stringify(roScan) + ')',
    /Duplicate Files scan/.test(JSON.stringify(roScan)));
  const roMove = await api('/move', {
    files: [volRoot + '/Backups/damaged/copy/archive.dat'], dest: volRoot + '/moved', preserve: false, tool: 'corrupted_files',
  });
  check('daemon refuses a move from the read-only tool (' + JSON.stringify(roMove) + ')',
    /report/.test(JSON.stringify(roMove)));
  check('and the file it refused to move is still there',
    fs.existsSync(volRoot + '/Backups/damaged/copy/archive.dat'));

  const rawInfo = await api('/info');
  const vol = rawInfo.volumes[0].path;
  const movedDir = vol + '/moved';
  fs.mkdirSync(movedDir, { recursive: true });

  // 2.1 — a reference folder passed with a trailing "/." must still protect
  // its files (the daemon stores refDirs cleaned)
  await api('/scan', {
    tool: 'duplicates', dirs: [vol + '/Backups'], refDirs: [vol + '/photo/.'],
    recurse: true, match: {},
  });
  let st = null;
  for (let i = 0; i < 80; i++) { st = await api('/state'); if (!st.running) break; await sleep(250); }
  check('raw duplicates scan finished', !!st && !st.running);
  check('daemon stores reference dirs cleaned', JSON.stringify(st.refDirs) === JSON.stringify([vol + '/photo']));
  const refMove = await api('/move', { files: [vol + '/photo/random.bin'], dest: movedDir, preserve: false });
  check('raw move of a file under "ref/." refused as read-only',
    refMove.moved.length === 0 && refMove.errors.length === 1 && /read-only reference/.test(refMove.errors[0].error));
  check('refused reference file still on disk', fs.existsSync(vol + '/photo/random.bin'));

  // 2.2 — requesting every file of a duplicate group moves all but one
  const pairA = vol + '/Backups/A/IMG_0001.JPG', pairB = vol + '/Backups/B/IMG_0001.JPG';
  const keepOne = await api('/move', { files: [pairA, pairB], dest: movedDir, preserve: false });
  check('raw move of a whole duplicate group moves exactly one file',
    keepOne.moved.length === 1 && keepOne.errors.length === 1 && /keeping one copy/.test(keepOne.errors[0].error));
  check('one copy of the group survives on disk', fs.existsSync(pairA) !== fs.existsSync(pairB));

  // 2.2b — the survivor stays protected after its group dissolved: a
  // follow-up request for the remaining copy (the two-request drain) must be
  // refused until the next duplicates scan.
  const survivor = fs.existsSync(pairA) ? pairA : pairB;
  const takeSurvivor = await api('/move', { files: [survivor], dest: movedDir, preserve: false });
  check('survivor of a drained group refused in a follow-up move',
    takeSurvivor.moved.length === 0 && /keeping one copy/.test((takeSurvivor.errors[0] || {}).error || ''));
  check('survivor still on disk', fs.existsSync(survivor));

  // 3.3 — a second export to the same folder must not overwrite the first
  const exp1 = await api('/export', { tool: 'duplicates', dest: movedDir });
  const stat1 = fs.statSync(exp1.file);
  const exp2 = await api('/export', { tool: 'duplicates', dest: movedDir });
  check('second export steps to a " (1)" name', exp2.file !== exp1.file && / \(1\)\.csv$/.test(exp2.file));
  check('first export untouched by second',
    fs.existsSync(exp1.file) && fs.statSync(exp1.file).mtimeMs === stat1.mtimeMs && fs.existsSync(exp2.file));

  // 3.2 — destinations and sources behind a symlink that resolves outside
  // the volumes are rejected
  const sneaky = vol + '/photo/sneaky';
  const linkMove = await api('/move', { files: [vol + '/Backups/A/clip.mov'], dest: sneaky, preserve: false });
  check('move into symlinked-out-of-volume dest rejected', /outside allowed volumes/.test(linkMove.error || ''));
  check('file not moved through the symlink', fs.existsSync(vol + '/Backups/A/clip.mov'));
  const linkExp = await api('/export', { tool: 'duplicates', dest: sneaky });
  check('export into symlinked-out-of-volume dest rejected', /outside allowed volumes/.test(linkExp.error || ''));
  const escMove = await api('/move', { files: [sneaky + '/escapee.bin'], dest: movedDir, preserve: false });
  check('move of a file behind a symlinked parent rejected',
    escMove.moved.length === 0 && /outside allowed volumes/.test((escMove.errors[0] || {}).error || ''));

  // ---- Symlink-alias and preserve-mode hardening ---------------------------
  // R2.2a — a reference file requested through a directory alias is refused
  // (the string-prefix guard alone would not match /photoalias/ to /photo)
  const aliasRef = await api('/move', { files: [vol + '/photoalias/random.bin'], dest: movedDir, preserve: false });
  check('move of a reference file via a symlink alias refused',
    aliasRef.moved.length === 0 && /read-only reference/.test((aliasRef.errors[0] || {}).error || ''));
  check('aliased reference file still on disk', fs.existsSync(vol + '/photo/random.bin'));

  // R2.2b — keep-one cannot be bypassed by naming one group member through
  // an alias: requesting A directly and B via Balias must still hold one back
  const kpA = vol + '/Backups/A/clip.mov', kpB = vol + '/Backups/B/clip.mov';
  const aliasKeep = await api('/move', { files: [kpA, vol + '/Balias/clip.mov'], dest: movedDir, preserve: false });
  check('keep-one holds when a duplicate group is drained via an alias',
    aliasKeep.moved.length === 1 && aliasKeep.errors.length === 1 && /keeping one copy/.test(aliasKeep.errors[0].error));
  check('one copy of the aliased group survives on disk', fs.existsSync(kpA) !== fs.existsSync(kpB));

  // (Preserve mode creates ONE folder per request at the destination — named
  // for the tool, enumerated to " (n)" when the name is taken — and mirrors
  // each source's folder chain inside it with CreateFolder(force_parent),
  // all run by DSM under its own path rules; the native symlink-component
  // guard was retired with the native mover.)
  check('a plain move lands flat, with no tool folder',
    fs.readdirSync(movedDir).every(
      // Conflicting Files is deliberately absent: it is read-only, so it has no
      // moveFolderNames entry and a folder by that name must never appear.
      (e) => !/^(Duplicates|Empty Folders|Empty Files|Temporary Files)( \(\d+\))?$/.test(e)));

  // R3 — a scan root that is a symlink escaping the volume is rejected
  // (read-only, but the scanner must not enumerate outside the volumes)
  const rootScan = await api('/scan', {
    tool: 'temp_files', dirs: [sneaky], refDirs: [], recurse: false, match: {},
  });
  check('scan root that is a symlink out of the volume rejected',
    /outside allowed volumes/.test(rootScan.error || ''));

  // R4 — the daemon moves only what a scan surfaced: an arbitrary in-volume
  // file (present on disk, never part of any cached result) is refused
  const unscanned = await api('/move', { files: [vol + '/spare/pres.bin'], dest: movedDir, preserve: false });
  check('move of a file no scan surfaced is refused',
    unscanned.moved.length === 0 && /not part of the current scan results/.test((unscanned.errors[0] || {}).error || ''));
  check('unscanned file still on disk', fs.existsSync(vol + '/spare/pres.bin'));

  // Preserve mode: ONE new tool-named folder per batch, each source's folder
  // chain mirrored inside it, original filenames kept. A regression here looks
  // like files landing directly under the destination, a second batch merging
  // into the first one's tree, or a file acquiring " (1)" because two sources
  // happened to share a basename.
  // The zero-byte pres* files are surfaced by an Empty Files scan first —
  // only what a scan surfaced is movable.
  await api('/scan', { tool: 'empty_files', dirs: [vol + '/spare'], refDirs: [], recurse: true, match: {} });
  for (let i = 0; i < 40; i++) { st = await api('/state'); if (!st.running) break; await sleep(250); }
  const pres3 = vol + '/moved3';
  fs.mkdirSync(pres3, { recursive: true });
  check('preserve destination starts empty', fs.readdirSync(pres3).length === 0);
  const presOk = await api('/move', {
    files: [vol + '/spare/pres2.bin', vol + '/spare/sub/pres2.bin'],
    dest: pres3, preserve: true, tool: 'empty_files',
  });
  check('preserve move reports the folder it created', presOk.folder === pres3 + '/Empty Files');
  check('preserve move mirrors both sources inside one tool folder',
    presOk.moved.length === 2 &&
    fs.existsSync(pres3 + '/Empty Files' + vol + '/spare/pres2.bin') &&
    fs.existsSync(pres3 + '/Empty Files' + vol + '/spare/sub/pres2.bin'));
  check('exactly one folder was created for the whole batch',
    fs.readdirSync(pres3).join('|') === 'Empty Files');
  // The move-progress key is what drives the busy dialog's bar, and its
  // ABSENCE is what stops the UI polling. A finished move that left one behind
  // would have the dialog poll for the life of the page.
  const afterMove = await api('/state');
  check('a finished move leaves no progress behind', !afterMove.move);
  check('same-basename sources kept their original names',
    !fs.existsSync(pres3 + '/Empty Files' + vol + '/spare/pres2 (1).bin'));

  // A second batch into the same destination must NOT merge into the first.
  const presTwo = await api('/move', {
    files: [vol + '/spare/pres3.bin'], dest: pres3, preserve: true, tool: 'empty_files',
  });
  check('a second batch gets its own numbered folder',
    presTwo.folder === pres3 + '/Empty Files (1)');
  check('the second batch landed in the numbered folder',
    presTwo.moved.length === 1 &&
    fs.existsSync(pres3 + '/Empty Files (1)' + vol + '/spare/pres3.bin'));
  check('the two batches did not merge',
    fs.readdirSync(pres3).sort().join('|') === 'Empty Files|Empty Files (1)');

  // Lazy allocation: a request whose every file is refused must leave no
  // empty folder behind (pres.bin was never surfaced by a scan).
  const moved4 = vol + '/moved4';
  fs.mkdirSync(moved4, { recursive: true });
  const noneMoved = await api('/move', {
    files: [vol + '/spare/pres.bin'], dest: moved4, preserve: true, tool: 'empty_files',
  });
  check('an all-refused preserve move creates no folder',
    noneMoved.moved.length === 0 && !noneMoved.folder && fs.readdirSync(moved4).length === 0);

  // The tool id decides a CREATED path component, so it is a map key on the
  // daemon and never a path fragment: a hostile id is refused outright.
  const badTool = await api('/move', {
    files: [vol + '/spare/pres.bin'], dest: moved4, preserve: true, tool: '../../etc',
  });
  check('preserve move with an unknown tool refused outright',
    /unknown tool/.test(badTool.error || ''));
  check('the refused tool id created nothing', fs.readdirSync(moved4).length === 0);

  // "Already in the destination" — the one guard whose position AND scope this
  // change altered. Without preserve it must still refuse, exactly as before.
  // With preserve it must NOT refuse: the batch folder is new, so the file
  // moves into it instead. That asymmetry is the whole of "batches never
  // merge", so both directions are pinned here.
  const inplaceSrc = vol + '/spare/inplace.bin';
  const selfDest = vol + '/spare';
  const inplaceFlat = await api('/move', {
    files: [inplaceSrc], dest: selfDest, preserve: false,
  });
  check('a plain move into the folder the file already sits in is refused',
    inplaceFlat.moved.length === 0 &&
    /already in the destination/.test((inplaceFlat.errors[0] || {}).error || ''));
  check('the refused in-place file is untouched', fs.existsSync(inplaceSrc));
  const inplacePres = await api('/move', {
    files: [inplaceSrc], dest: selfDest, preserve: true, tool: 'empty_files',
  });
  check('the same move WITH preserve lands in the new tool folder',
    inplacePres.moved.length === 1 &&
    inplacePres.folder === selfDest + '/Empty Files' &&
    fs.existsSync(selfDest + '/Empty Files' + vol + '/spare/inplace.bin') &&
    !fs.existsSync(inplaceSrc));

  // R5 — identity check: a file modified since the scan that surfaced it is
  // no longer what the scan saw, and must be refused rather than moved.
  // The ARM runner mutates the fixture from the host while the daemon reads
  // it inside a container, and virtiofs attribute caching can briefly show
  // the daemon a stale size — so wait until the daemon's own (mock) File
  // Station observes the append before asking it to act on the file.
  fs.appendFileSync(vol + '/spare/ident.bin', 'grown since the scan');
  const fsInfo = async (share) => {
    const res = await fetch('http://localhost:' + PORT + '/webapi/entry.cgi', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: 'api=SYNO.FileStation.List&version=2&method=getinfo&path=' +
        encodeURIComponent(JSON.stringify([share])),
    });
    const j = await res.json();
    return (j.data && j.data.files && j.data.files[0]) || {};
  };
  for (let i = 0; i < 40; i++) {
    const info = await fsInfo('/spare/ident.bin');
    if (info.additional && info.additional.size > 0) break;
    await sleep(250);
  }
  const changed = await api('/move', { files: [vol + '/spare/ident.bin'], dest: pres3, preserve: false });
  check('move of a file changed since its scan is refused',
    changed.moved.length === 0 && /changed since the scan/.test((changed.errors[0] || {}).error || ''));
  check('changed file still on disk', fs.existsSync(vol + '/spare/ident.bin'));

  // The same refusal in PRESERVE mode. This one matters for a reason the
  // all-refused test above cannot cover: that refusal happens in handleMove,
  // before execMoveFS is entered at all, whereas the identity check lives
  // INSIDE execMoveFS. The batch folder is allocated on the last statement
  // before the move, so every refusal in that function has to sit above it —
  // move the allocation up and this is the assertion that notices.
  const moved5 = vol + '/moved5';
  fs.mkdirSync(moved5, { recursive: true });
  const changedPres = await api('/move', {
    files: [vol + '/spare/ident.bin'], dest: moved5, preserve: true, tool: 'empty_files',
  });
  check('a preserve move refused inside execMoveFS creates no folder',
    changedPres.moved.length === 0 &&
    /changed since the scan/.test((changedPres.errors[0] || {}).error || '') &&
    !changedPres.folder && fs.readdirSync(moved5).length === 0);

  // A file File Station cannot address is refused BEFORE File Station is
  // asked, with a reason that does not claim the file is missing — asking
  // produces "no such file or folder" for a file the scan just read.
  await api('/scan', { tool: 'temp_files', dirs: [vol + '/photo/dsonly'], refDirs: [], recurse: true, match: {} });
  for (let i = 0; i < 40; i++) { st = await api('/state'); if (!st.running) break; await sleep(250); }
  const dsRows = await api('/results?tool=temp_files');
  const dsRow = (dsRows.files || []).find((f) => f.name === '.DS_Store');
  check('the unaddressable file was scanned and flagged', !!dsRow && dsRow.nomove === true);
  // the AppleDouble half of the rule, on a file that really exists
  const adRow = (dsRows.files || []).find((f) => f.name === '._sidecar');
  check('an AppleDouble sidecar is flagged too', !!adRow && adRow.nomove === true);
  const dsMv = await api('/move', {
    tool: 'temp_files', files: [dsRow.path + '/' + dsRow.name], dest: vol + '/moved', preserve: false,
  });
  const dsErr = (dsMv.errors[0] || {}).error || '';
  check('daemon refuses an unaddressable file', dsMv.moved.length === 0 && dsMv.errors.length === 1);
  check('and says why, without claiming the file does not exist',
    /File Station cannot see/.test(dsErr) && !/no such file/.test(dsErr));
  check('the refused file is still on disk', fs.existsSync(vol + '/photo/dsonly/.DS_Store'));

  // A junk-only "empty" folder moves as a folder, junk riding along inside,
  // and the daemon prunes rows UNDER the moved directory (its junk may hold
  // temp_files rows, which would otherwise point at gone paths). Scan both
  // tools first so both rows exist in the cache.
  await api('/scan', { tool: 'temp_files', dirs: [vol + '/photo/junkonly'], refDirs: [], recurse: true, match: {} });
  for (let i = 0; i < 40; i++) { st = await api('/state'); if (!st.running) break; await sleep(250); }
  await api('/scan', { tool: 'empty_folders', dirs: [vol + '/photo'], refDirs: [], recurse: true, match: {} });
  for (let i = 0; i < 40; i++) { st = await api('/state'); if (!st.running) break; await sleep(250); }
  const junkDest = vol + '/moved-junk';
  fs.mkdirSync(junkDest, { recursive: true });
  // Count the rows the move must clear BEFORE it runs. Without this the
  // "no row mentions /junkonly" assertion below passes vacuously on an empty
  // list — and every temp row here IS under junkonly, so after a correct
  // prune the list is empty and the check would prove nothing on its own.
  const tempBeforeJunkMv = await api('/results?tool=temp_files');
  const junkRowsBefore = (tempBeforeJunkMv.files || [])
    .filter((f) => (f.path || '').indexOf('/junkonly') >= 0).length;
  check('the junk-only folder held temp rows before the move', junkRowsBefore > 0);
  const junkMv = await api('/move', {
    tool: 'empty_folders', files: [vol + '/photo/junkonly'], dest: junkDest, preserve: false,
  });
  if (junkMv.moved.length !== 1 || junkMv.errors.length !== 0) {
    // A refusal here is otherwise invisible: the assertion only reports a
    // boolean, and the reason is the whole diagnosis.
    console.log('      junk move payload: ' + JSON.stringify(junkMv));
    const efNow = await api('/results?tool=empty_folders');
    console.log('      empty_folders rows: ' +
      JSON.stringify((efNow.files || []).map((f) => f.path + '/' + f.name)));
  }
  check('junk-only folder moves as a folder', junkMv.moved.length === 1 && junkMv.errors.length === 0);
  check('the junk rode along inside the moved folder',
    fs.existsSync(junkDest + '/junkonly/Thumbs.db') && !fs.existsSync(vol + '/photo/junkonly'));
  const tempAfterJunkMv = await api('/results?tool=temp_files');
  // An empty result must serialize as [], never null: raw callers read
  // .files.length, and the paged POST beside this endpoint guarantees it.
  check('an emptied result set is [] and not null', Array.isArray(tempAfterJunkMv.files));
  check('temp rows under the moved folder are pruned',
    (tempAfterJunkMv.files || []).every((f) => (f.path || '').indexOf('/junkonly') < 0));

  // R6 — an aliased scope must not fabricate phantom duplicate pairs: Balias
  // resolves inside Backups, so the walk de-overlaps it (a walker visiting the
  // same files twice under both display paths would turn every pair into a
  // triple).
  await api('/scan', { tool: 'duplicates', dirs: [vol + '/Backups', vol + '/Balias'], refDirs: [], recurse: true, match: {} });
  for (let i = 0; i < 80; i++) { st = await api('/state'); if (!st.running) break; await sleep(250); }
  const aliasDump = await api('/results?tool=duplicates');
  let maxGroupSize = 0;
  aliasDump.groups.forEach((g) => { maxGroupSize = Math.max(maxGroupSize, g.files.length); });
  check('aliased scope is de-overlapped (no phantom groups)', aliasDump.groups.length >= 100 && maxGroupSize === 2);

  // ---- paged results API (raw) --------------------------------------------
  // POST pages with whole-set totals; the legacy GET dump stays complete.
  const paged = await api('/results', { tool: 'duplicates', offset: 0, limit: 5 });
  check('raw paged POST returns one page plus totals',
    paged.scanned === true && paged.groups.length === 5 && paged.total.groups >= 100);
  const lastPage = await api('/results', { tool: 'duplicates', offset: paged.total.groups - 2, limit: 5 });
  check('raw paged POST clamps the final page', lastPage.groups.length === 2);
  const dump = await api('/results?tool=duplicates');
  check('legacy GET dump still returns every group',
    dump.scanned === true && dump.groups.length === paged.total.groups);

  // ---- move-time verification: the moved-but-failed contract --------------
  // The dev mock corrupts any moved file named "corrupt-transit" right after
  // the rename lands (dev.go hook): damage in transit, end to end. The daemon
  // must report the mismatch, keep the file OUT of moved[], and still prune
  // the row — the source is gone, and a lingering row would point at nothing
  // and haunt keep-one. Placed last: the rescan below replaces the duplicates
  // results earlier sections asserted against.
  {
    fsm.mkdirSync(vol + '/spare/ct-src', { recursive: true });
    fsm.mkdirSync(vol + '/spare/ct-dest', { recursive: true });
    const ctA = vol + '/spare/ct-src/clip.corrupt-transit.bin';
    const ctB = vol + '/spare/ct-src/keep.bin';
    const ctBody = Buffer.alloc(4096, 7);
    fsm.writeFileSync(ctA, ctBody);
    fsm.writeFileSync(ctB, ctBody);
    await api('/scan', { tool: 'duplicates', dirs: [vol + '/spare/ct-src'], refDirs: [], recurse: true, match: {} });
    for (let i = 0; i < 100; i++) {
      const st = await api('/state');
      if (st && st.running === false) break;
      await sleep(100);
    }
    const pre = await api('/results?tool=duplicates');
    check('the transit fixture pair grouped', pre.groups.length === 1 && pre.groups[0].files.length === 2);
    const mv = await api('/move', { files: [ctA], dest: vol + '/spare/ct-dest', preserve: false, tool: 'duplicates', verify: true });
    check('a corrupted-in-transit move reports the mismatch',
      mv.errors.length === 1 && /does not match the original content/.test(mv.errors[0].error));
    check('the damaged move is not counted as moved', mv.moved.length === 0);
    check('the source is gone and the corrupted copy sits at the destination',
      !fsm.existsSync(ctA) && fsm.existsSync(vol + '/spare/ct-dest/clip.corrupt-transit.bin'));
    const post = await api('/results?tool=duplicates');
    check('the moved-but-unverified row is pruned from the results',
      JSON.stringify(post.groups).indexOf('corrupt-transit') < 0);
    // Same damage with verification OFF passes silently — the checkbox is a
    // real switch, not decoration.
    fsm.writeFileSync(ctA, ctBody);
    await api('/scan', { tool: 'duplicates', dirs: [vol + '/spare/ct-src'], refDirs: [], recurse: true, match: {} });
    for (let i = 0; i < 100; i++) {
      const st = await api('/state');
      if (st && st.running === false) break;
      await sleep(100);
    }
    const mv2 = await api('/move', { files: [ctA], dest: vol + '/spare/ct-dest', preserve: true, tool: 'duplicates', verify: false });
    check('the same damage with verification off moves without complaint',
      mv2.errors.length === 0 && mv2.moved.length === 1);
    // Leave at least one duplicate group in the stored results: run.sh's
    // restart section asserts restored results still carry groups.
    fsm.writeFileSync(ctA, ctBody);
    await api('/scan', { tool: 'duplicates', dirs: [vol + '/spare/ct-src'], refDirs: [], recurse: true, match: {} });
    for (let i = 0; i < 100; i++) {
      const st = await api('/state');
      if (st && st.running === false) break;
      await sleep(100);
    }
  }

  console.log(failures ? failures + ' FAILURES' : 'ALL PASSED');
  process.exit(failures ? 1 : 0);
})().catch((e) => {
  console.error('FATAL', e);
  process.exit(2);
});
