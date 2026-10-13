/* termcp Web UI — the extension panel.
 *
 * The right-hand column is the connections panel's other side. NetHub lists the
 * machines Termcp can dial; this lists the trimmed sessions Termcp keeps warm
 * for whoever is driving it (an agent over MCP, mostly). A row is a template's
 * runtime verdict — up, trying, down with a reason — plus the one handle an up
 * extension is driven through.
 *
 * The panel owns no state of its own. The socket delivers the whole list on
 * every change, because the reconciler broadcasts after each state transition,
 * so renderExtensionGrid is a full repaint with nothing to reconcile against.
 * An empty list keeps the column: it is where a new extension is created from,
 * and a panel that vanishes when it is empty is a panel the author cannot open
 * to author with.
 *
 * Authoring goes through the same store the extension MCP tool writes to — the
 * REST routes are that store's other caller — so a template saved here and one
 * saved by an agent are indistinguishable, and the two surfaces cannot drift
 * apart on what a valid template is.
 *
 * Loaded as a plain synchronous <script> like every other module: the
 * declarations land in the shared global scope, and the socket's frame handler
 * is what calls into it.
 */

// The last frame delivered, kept only so a language switch can repaint the
// cards from the same rows rather than from a cache of its own.
var _extRows = null;

// Reconciler statuses, in the order the panel lists them: what can be used
// first, what is being brought up, what needs attention, what was put away.
var EXT_STATUS_ORDER = ['available', 'probing', 'unavailable', 'disabled'];

// The lamp class is keyed on the status, so a status the panel has never seen
// falls back to the down lamp rather than to no dot at all.
var EXT_LAMP_CLASSES = {
  available: 'is-online',
  probing: 'is-connecting',
  unavailable: 'is-offline',
  disabled: 'is-disabled'
};

// A clockwise arrow, read as "try again": the only per-card action.
var SVG_EXT_RETRY = '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M14 8a6 6 0 11-1.9-4.4"/><path d="M14 1.6V4.6H11"/></svg>';

// A pencil, read as "edit the template".
var SVG_EXT_EDIT = '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.5 2.5l2 2L6 12H4v-2z"/><path d="M9.5 4.5l2 2"/></svg>';

// A trash can, read as "delete the template".
var SVG_EXT_DELETE = '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 4.5h10"/><path d="M6.5 4.5V3h3v1.5"/><path d="M4.5 4.5l.6 8.5h5.8l.6-8.5"/></svg>';

function extLampClass(status) {
  return EXT_LAMP_CLASSES[status] || 'is-offline';
}

/* The catalog keys are spelt out in the switches rather than concatenated
   (`t('ext.status.' + status)`) so a reader grepping for a string finds the
   panel, and so a status the panel has never seen falls back to the word for
   "unavailable" instead of a raw key on screen. */
function extStatusLabel(status) {
  switch (status) {
    case 'available': return t('ext.status.available');
    case 'probing': return t('ext.status.probing');
    case 'disabled': return t('ext.status.disabled');
    default: return t('ext.status.unavailable');
  }
}

/* The three presets the config ships; anything else is a custom template and is
   shown as its own kind verbatim rather than mislabelled. */
function extKindLabel(kind) {
  switch (kind) {
    case 'mcp': return t('ext.kind.mcp');
    case 'skill': return t('ext.kind.skill');
    case 'prompt': return t('ext.kind.prompt');
    default: return kind || '';
  }
}

// The forward modal already names the three directions; reusing those strings
// keeps one vocabulary for one concept, and an unknown kind still has a label.
function extForwardLabel(kind) {
  var key = 'modal.forward.dir.' + kind;
  var s = t(key);
  return (s === key) ? (kind || '') : s;
}

/* Sort by what the reader is here for, then by name: an extension that is up is
   above one that is trying, which is above one that is down, and a disabled one
   is out of the way at the bottom. */
function extRowOrder(a, b) {
  var ai = EXT_STATUS_ORDER.indexOf(a.status);
  var bi = EXT_STATUS_ORDER.indexOf(b.status);
  if (ai < 0) ai = EXT_STATUS_ORDER.length;
  if (bi < 0) bi = EXT_STATUS_ORDER.length;
  if (ai !== bi) return ai - bi;
  return String(a.name || '').localeCompare(String(b.name || ''));
}

function renderExtensionGrid(rows) {
  _extRows = rows || [];
  var panel = document.getElementById('extpanel');
  var grid = document.getElementById('ext-grid');
  var rail = document.getElementById('extpanel-rail');
  var frame = document.querySelector('.workspace-frame');
  if (!panel || !grid || !frame) return;
  var list = _extRows.slice().sort(extRowOrder);
  /* The column is permanent in the markup (.ext-on is on the frame from the
     start), so an empty list is laid out exactly like a full one and the empty
     state is where the first extension is created from — a panel that
     disappeared when empty was a panel the author could not open to author
     with. */
  grid.innerHTML = '';
  if (list.length === 0) {
    grid.appendChild(renderExtensionEmpty());
  } else {
    list.forEach(function (ext) {
      grid.appendChild(renderExtensionCard(ext));
    });
  }
  /* The rail is the collapsed column's whole content, so it is repainted from
     the same frame and keeps the same order as the cards beside it. */
  if (rail) {
    rail.innerHTML = '';
    list.forEach(function (ext) {
      rail.appendChild(renderExtensionRailItem(ext));
    });
  }
  /* A repaint preserves the state the page is in rather than re-deriving it:
     the stored preference says what the desktop sidebar should be, and reading
     it here would reopen a drawer that arrived closed below the breakpoint. */
  setExtensionsCollapsed(extensionsCollapsed(), false);
}

/* The empty state is the panel's reason to exist when nothing is configured:
   it tells the author what an extension is and hands them the tool to make
   one. A socket frame that arrives before the first paint renders it too, so
   the column is never a blank track. */
function renderExtensionEmpty() {
  var row = document.createElement('div');
  row.className = 'ext-empty';
  row.innerHTML =
    '<p class="ext-empty-text">' + escapeHtml(t('ext.empty.text')) + '</p>' +
    '<button type="button" class="ext-empty-btn" id="ext-empty-add">' +
    escapeHtml(t('ext.empty.button')) + '</button>';
  var btn = row.querySelector('#ext-empty-add');
  if (btn) btn.addEventListener('click', function () { openExtensionEditor(null); });
  return row;
}

function renderExtensionCard(ext) {
  var tile = document.createElement('div');
  var lamp = extLampClass(ext.status);
  // An available extension with a session is the only row that opens anything.
  // One with no shell (a skill preset, driven over SFTP) has no terminal to
  // raise, so it stays a report rather than becoming a dead affordance.
  var openable = ext.status === 'available' && !!ext.session_id && !!ext.shell_id;
  var retryable = ext.status === 'unavailable' || ext.status === 'probing';
  tile.className = 'conn-tile entry-card ext-card' + (openable ? ' ext-openable' : '');
  tile.setAttribute('data-ext-name', ext.name || '');
  tile.setAttribute('data-ext-status', ext.status || '');
  var name = ext.name || '';
  var retryBtn = retryable
    ? '<button type="button" class="ext-retry-btn" title="' + escapeHtml(t('ext.retry.title')) +
      '" aria-label="' + escapeHtml(t('ext.retry.title')) + '">' + SVG_EXT_RETRY + '</button>'
    : '';
  // The row's authoring actions. Every card carries them, so an extension is
  // fixed where it is reported — a down extension with a bad template is one
  // click from the edit that would correct it.
  var editBtn = '<button type="button" class="ext-edit-btn" title="' + escapeHtml(t('ext.edit.title')) +
    '" aria-label="' + escapeHtml(t('ext.edit.title')) + '">' + SVG_EXT_EDIT + '</button>';
  var delBtn = '<button type="button" class="ext-del-btn" title="' + escapeHtml(t('ext.delete.title')) +
    '" aria-label="' + escapeHtml(t('ext.delete.title')) + '">' + SVG_EXT_DELETE + '</button>';
  var meta =
    '<span class="ext-kind">' + escapeHtml(extKindLabel(ext.kind)) + '</span>' +
    '<span class="node-sep" aria-hidden="true">·</span>' +
    '<span class="node-status ' + lamp + '">' + escapeHtml(extStatusLabel(ext.status)) + '</span>';
  var target = ext.target ? '<div class="node-addr">' + escapeHtml(ext.target) + '</div>' : '';
  var fw = '';
  (ext.forwards || []).forEach(function (f) {
    if (!f.listen_addr) return;
    fw += '<div class="ext-fwd" title="' + escapeHtml(t('ext.fwd.title', { kind: extForwardLabel(f.kind) })) + '">' +
      escapeHtml(f.listen_addr) + '</div>';
  });
  // One line, not the history: the verdict is what the panel is for, and the
  // detail endpoint serves the rest.
  var fail = '';
  if (ext.failure && ext.failure.reason) {
    fail = '<div class="ext-fail" title="' + escapeHtml(t('ext.fail.title', { stage: ext.failure.stage || '' })) + '">' +
      escapeHtml(ext.failure.reason) + '</div>';
  }
  tile.innerHTML =
    '<div class="entry-card-inner">' +
    '<span class="node-lamp ' + lamp + '" aria-hidden="true"></span>' +
    '<div class="entry-card-main">' +
    '<div class="conn-nm-row">' +
    '<span class="conn-nm-cluster">' +
    '<span class="conn-nm">' + escapeHtml(name) + '</span>' +
    '</span>' +
    retryBtn +
    editBtn +
    delBtn +
    '</div>' +
    '<div class="node-meta">' + meta + '</div>' +
    target +
    fw +
    fail +
    '</div>' +
    '</div>';
  var inner = tile.querySelector('.entry-card-inner');
  if (inner) {
    inner.title = openable
      ? t('ext.tip.open')
      : (ext.status === 'available' ? t('ext.tip.driven') : extStatusLabel(ext.status));
    inner.addEventListener('click', function (e) {
      if (e.target.closest('.ext-retry-btn, .ext-edit-btn, .ext-del-btn')) return;
      if (!openable) return;
      /* The session may be serving several extensions — one connection per host —
         so the channel is named too and the window lands on this extension's
         terminal rather than on whichever channel was last used. */
      focusSessionWindow(name, ext.session_id, windowPositionFromClick(e), { shell: ext.shell_id });
    });
  }
  var btn = tile.querySelector('.ext-retry-btn');
  if (btn) {
    btn.addEventListener('click', function (e) {
      e.stopPropagation();
      retryExtension(name, btn);
    });
  }
  var editEl = tile.querySelector('.ext-edit-btn');
  if (editEl) {
    editEl.addEventListener('click', function (e) {
      e.stopPropagation();
      openExtensionEditor(name);
    });
  }
  var delEl = tile.querySelector('.ext-del-btn');
  if (delEl) {
    delEl.addEventListener('click', function (e) {
      e.stopPropagation();
      deleteExtension(name, delEl);
    });
  }
  return tile;
}

/* Ask the reconciler to try again now. The human's nudge is the same call the
   agent's extension(action=retry) makes, so the two cannot drift apart in what
   they are allowed to do. The card stays pressed until the next frame lands;
   the repaint is the receipt. */
function retryExtension(name, btn) {
  if (!name) return;
  if (btn) btn.disabled = true;
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name) + '/retry', { method: 'POST' })
    .then(function (r) {
      if (!r.ok) {
        return r.json().catch(function () { return {}; }).then(function (j) {
          throw new Error(j.error || r.status);
        });
      }
    })
    .catch(function (err) {
      if (btn) btn.disabled = false;
      console.error('extension retry failed: ' + ((err && err.message) || err));
    });
}

/* The collapsed column's row: a lamp and a name, and nothing else. The status
   is the whole point — the rail exists so "which extensions are up" still has
   an answer when the cards are put away — and the name is what the tooltip
   holds, because a lamp alone will not say which machine it is. */
function renderExtensionRailItem(ext) {
  var btn = document.createElement('button');
  var lamp = extLampClass(ext.status);
  var name = ext.name || '';
  btn.type = 'button';
  btn.className = 'extpanel-rail-item';
  btn.title = name + ' — ' + extStatusLabel(ext.status);
  btn.setAttribute('aria-label', name + ', ' + extStatusLabel(ext.status));
  btn.innerHTML = '<span class="extpanel-rail-lamp ' + lamp + '" aria-hidden="true"></span>';
  btn.addEventListener('click', function (e) {
    e.stopPropagation();
    setExtensionsCollapsed(false, true);
  });
  return btn;
}

/* ---- Collapse state -------------------------------------------------------
 * The same three widths and one manual preference NetHub keeps, and for the
 * same reason: this is the connections panel's other side, so it answers the
 * same question the same way.
 *
 *   wide (>=1200px)   expanded by default
 *   medium (800-1199) collapsed (rail) by default
 *   narrow (<800px)   the panel is a drawer, and "collapsed" means closed
 *
 * The key is the panel's own; NetHub's preference does not decide this column,
 * and vice versa. Only a toggle above the drawer breakpoint writes it — below it
 * the panel is an overlay, and the overlay's open and close are the page's
 * business rather than the sidebar's preference. But the two drawers share one
 * scrim and one page, so opening one closes the other — two overlays at once is
 * two frosted panes over a page that cannot scroll behind either. */
var EXT_STATE_KEY = 'termcp.extensions';

function storedExtensionsCollapsed() {
  var v = null;
  try { v = localStorage.getItem(EXT_STATE_KEY); } catch (e) {}
  if (v === 'collapsed') return true;
  if (v === 'expanded') return false;
  return null;
}

/* Mirrors the stylesheet's breakpoint. Below it the body is fixed and slides,
   so the state means closed rather than railed — and the frame's collapsed
   class is kept off it, or the third grid track would still be 56px of rail on
   a page that no longer has grid columns at all. */
function extIsDrawer() {
  return (window.innerWidth || 0) < 800;
}

function extDefaultCollapsed() {
  return (window.innerWidth || 0) < 1200;
}

function setExtensionsCollapsed(collapsed, persist) {
  var frame = document.querySelector('.workspace-frame');
  var body = document.getElementById('extpanel-body');
  var trigger = document.getElementById('extpanel-toggle');
  if (!frame || !body) return;
  collapsed = !!collapsed;
  var drawer = extIsDrawer();
  /* The two drawers share one scrim and one page, so the one being opened puts
     the other away. The call is guarded by "am I opening" rather than fired on
     every state write, and closing never calls across, so the pair cannot
     recurse into each other. */
  if (drawer && !collapsed && !netHubCollapsed()) setNetHubCollapsed(true, false);
  frame.classList.toggle('ext-collapsed', collapsed && !drawer);
  body.classList.toggle('drawer-open', drawer && !collapsed);
  /* The scrim and the page lock follow BOTH drawers (paintDrawerScrim in
     dialogs.js), so collapsing this panel no longer drops the overlay the
     connections drawer is still depending on. */
  if (typeof paintDrawerScrim === 'function') paintDrawerScrim();
  if (trigger) trigger.setAttribute('aria-expanded', String(!collapsed));
  /* The drawer scope never writes the preference: below the breakpoint the
     panel is an overlay, and dismissing an overlay is not a statement about how
     the desktop sidebar should sit. Writing it here would rail a column the
     wide screen later arrives at — the panel found collapsed for a decision
     made about a different shape of the same page. The overlay's state lives in
     the page, and the sidebar keeps its own. */
  if (persist && !drawer) {
    try { localStorage.setItem(EXT_STATE_KEY, collapsed ? 'collapsed' : 'expanded'); } catch (e) {}
  }
}

function extensionsCollapsed() {
  var frame = document.querySelector('.workspace-frame');
  var body = document.getElementById('extpanel-body');
  if (extIsDrawer()) return !(body && body.classList.contains('drawer-open'));
  return !!(frame && frame.classList.contains('ext-collapsed'));
}

function currentExtensionsCollapsed() {
  var stored = storedExtensionsCollapsed();
  return stored === null ? extDefaultCollapsed() : stored;
}

/* A drawer is an overlay over the page, and an overlay does not open itself.
   "Expanded" was stored on a wide screen, where it means the sidebar is out;
   carried below the breakpoint it means a drawer over a page that cannot scroll
   behind it, with a scrim the user never asked for. The stored value still
   decides the desktop sidebar — this is the state the panel arrives in, not a
   rewrite of the preference. */
function initialExtensionsCollapsed() {
  return extIsDrawer() ? true : currentExtensionsCollapsed();
}

function toggleExtensions() {
  setExtensionsCollapsed(!extensionsCollapsed(), true);
}

/* The drawer's close key and the drawer-close button both reach for this, so
   the panel has one door from the inside as well as out. */
window.termcpToggleExtensions = function (open) {
  setExtensionsCollapsed(open === false, true);
};

/* A viewport change can move the page across a breakpoint in either direction,
   and the stored preference is re-applied, never rewritten — a resize is not a
   decision. Only a crossing is acted on: a resize that stays on one side of the
   breakpoint changes the panel's geometry but not its state, and re-applying
   would close a drawer the user is in the middle of using. Crossing into the
   drawer scope starts closed, so an overlay never opens itself; crossing back
   out keeps a drawer the user had open as the expanded column it becomes,
   rather than re-deriving it from storage — a rotation into a width whose
   default is the rail would otherwise put away a panel the user was reading. */
var _extWasDrawer = extIsDrawer();
var _extResizeTimer = 0;
window.addEventListener('resize', function () {
  if (_extResizeTimer) clearTimeout(_extResizeTimer);
  _extResizeTimer = setTimeout(function () {
    _extResizeTimer = 0;
    var drawer = extIsDrawer();
    if (drawer === _extWasDrawer) return;
    var wasDrawer = _extWasDrawer;
    _extWasDrawer = drawer;
    if (drawer) {
      setExtensionsCollapsed(initialExtensionsCollapsed(), false);
      return;
    }
    var panel = document.getElementById('extpanel-body');
    var wasOpen = wasDrawer && panel && panel.classList.contains('drawer-open');
    setExtensionsCollapsed(wasOpen ? false : currentExtensionsCollapsed(), false);
  }, 120);
});

(function () {
  var trigger = document.getElementById('extpanel-toggle');
  if (!trigger) return;
  trigger.addEventListener('click', function (e) {
    e.stopImmediatePropagation();
    e.preventDefault();
    toggleExtensions();
  }, true);
  trigger.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.stopImmediatePropagation();
    e.preventDefault();
    toggleExtensions();
  }, true);

  /* The scrim and Escape close the overlay only. On desktop the scrim is not in
     the layout and the body is a column, so neither can put away a rail the
     user chose — the same guard NetHub's own handlers carry. */
  var scrim = document.querySelector('.drawer-scrim');
  if (scrim) scrim.addEventListener('click', function () { setExtensionsCollapsed(true, false); });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    if (!extIsDrawer()) return;
    var body = document.getElementById('extpanel-body');
    if (!body || !body.classList.contains('drawer-open')) return;
    e.stopImmediatePropagation();
    setExtensionsCollapsed(true, false);
  }, true);
})();

setExtensionsCollapsed(initialExtensionsCollapsed(), false);

/* The socket frame is the panel's usual painter, but it only carries the key
   when a reconciler is running — a Termcp started without one sends nothing,
   and the column is in the markup whether or not anything answers. This fills
   that gap once: it runs after the socket connects, paints nothing the frame
   has not already painted (_extRows stays null until someone does), and a
   failure still lands as an empty column rather than as an unrendered one. */
function loadExtensionsOnce() {
  if (_extRows) return;
  fetch(apiPath('/api/extensions'))
    .then(function (r) { return r.json(); })
    .then(function (j) { renderExtensionGrid(j.extensions || []); })
    .catch(function () { renderExtensionGrid([]); });
}

onLangChange(function () {
  if (_extRows) renderExtensionGrid(_extRows);
});

/* ---- Authoring -------------------------------------------------------------
 * The panel's other half. These write the same store the extension MCP tool
 * writes to, through the REST routes that store's reconciler serves, so a
 * template authored here and one authored by an agent are one file on disk and
 * one row in this panel.
 *
 * The form mirrors the authoring layer's semantics: every field sends its value
 * on save, and an emptied field sends an empty array or string, which the
 * layer reads as "clear" rather than "leave alone". */
var _extEditing = null;

function extModalEl(id) { return document.getElementById(id); }

/* Lines to a whitelist and back: the field is a textarea because a whitelist is
 * a list, and one entry per line is the shape an author can scan and edit. An
 * empty textarea is an empty array, which is the "clear it" intent. */
function extLinesToText(list) {
  return (list || []).join('\n');
}

function extTextToLines(text) {
  return text.split('\n').map(function (s) { return s.trim(); })
    .filter(function (s) { return s.length > 0; });
}

/* The target list is the same set of profiles the connection drawer offers,
 * because an extension's template dials one of them. "internal" is the loopback
 * the skill and prompt presets reach through. */
function extFillTargetSelect(selected) {
  var sel = extModalEl('ext-f-target');
  if (!sel) return;
  var keep = selected;
  fetch(apiPath('/api/connections')).then(function (r) { return r.json(); }).then(function (j) {
    var names = (j.connections || []).map(function (c) { return c.name; })
      .filter(function (n) { return !!n; });
    if (names.indexOf('internal') < 0) names.unshift('internal');
    sel.innerHTML = names.map(function (n) {
      return '<option value="' + escapeHtml(n) + '"' + (n === keep ? ' selected' : '') + '>' +
        escapeHtml(n) + '</option>';
    }).join('');
  }).catch(function () {});
}

/* openExtensionEditor loads a template into the form. A null name is a create:
 * the form starts from the mcp preset, the conventional first choice, and the
 * name field is editable because the file does not exist yet. Editing an
 * existing template locks the name — it is the directory the file lives in,
 * and renaming is a move the authoring layer does not offer either caller. */
function openExtensionEditor(name) {
  var modal = extModalEl('modal-ext');
  if (!modal) return;
  _extEditing = name || null;
  var title = extModalEl('modal-ext-title');
  var err = extModalEl('modal-ext-err');
  if (err) { err.style.display = 'none'; err.textContent = ''; }
  if (name) {
    if (title) title.textContent = t('ext.modal.edit', { name: name });
    fetch(apiPath('/api/extensions/') + encodeURIComponent(name))
      .then(function (r) { return r.json(); })
      .then(function (j) {
        /* The detail endpoint splits the template: the fields the reconciler
         * reports on (name, kind, target, description, enabled) ride under
         * "extension" with the live status, and the session shape (command,
         * args, mode, allow, paths, notes) sits at the top. Both are read, so a
         * template saved by the agent and one saved here open to the same form. */
        var live = j.extension || {};
        extPaintForm({
          name: name,
          kind: live.kind || j.kind,
          target: live.target || j.target,
          description: live.description || j.description || '',
          command: j.command || '',
          args: j.args || [],
          mode: j.mode || '',
          allow: j.allow || [],
          paths: j.paths || [],
          notes: j.notes || '',
          enabled: live.enabled !== false
        });
      })
      .catch(function () {
        showExtError(t('ext.modal.loadFailed'));
      });
  } else {
    if (title) title.textContent = t('ext.modal.title');
    var preset = extPresetFor('mcp');
    preset.name = '';
    extPaintForm(preset);
  }
  var nameEl = extModalEl('ext-f-name');
  if (nameEl) nameEl.readOnly = !!name;
  extFillTargetSelect(name ? null : 'internal');
  modal.classList.remove('hidden');
  if (nameEl && !name) nameEl.focus();
}

function extPaintForm(ext) {
  var set = function (id, v) {
    var el = extModalEl(id);
    if (!el) return;
    if (el.type === 'checkbox') el.checked = !!v;
    else el.value = v == null ? '' : String(v);
  };
  set('ext-f-name', ext.name || '');
  set('ext-f-kind', ext.kind || 'custom');
  set('ext-f-desc', ext.description || '');
  set('ext-f-command', ext.command || '');
  set('ext-f-args', extLinesToText(ext.args));
  set('ext-f-mode', ext.mode || '');
  set('ext-f-allow', extLinesToText(ext.allow));
  set('ext-f-paths', extLinesToText(ext.paths));
  set('ext-f-notes', ext.notes || '');
  set('ext-f-enabled', ext.enabled !== false);
}

/* The preset the form starts a create from, chosen by the kind field. The
 * server's authoring layer applies the same presets; this mirrors them only so
 * the form can show the author what they are about to get before they save. */
function extPresetFor(kind) {
  switch (kind) {
    case 'mcp':
      return { kind: 'mcp', mode: 'pipe', command: 'npx', args: ['-y', 'example-mcp-server'],
               allow: ['shell_input', 'shell_output'], paths: [], notes: '', enabled: true };
    case 'skill':
      return { kind: 'skill', mode: '', command: '', args: [],
               allow: ['file_read', 'file_stat'], paths: [], notes: '', enabled: true };
    case 'prompt':
      return { kind: 'prompt', mode: 'pty', command: '', args: [],
               allow: [], paths: [], notes: '', enabled: true };
    default:
      return { kind: 'custom', mode: '', command: '', args: [],
               allow: [], paths: [], notes: '', enabled: true };
  }
}

/* Changing the kind re-seeds the conventional fields the way the authoring
 * layer's preset does. Anything the author already filled in is kept: a reseed
 * is a head start, not an overwrite. */
function extSeedFromKind(kind) {
  var preset = extPresetFor(kind);
  var take = function (id, v) {
    var el = extModalEl(id);
    if (el && !el.value.trim()) el.value = v == null ? '' : String(v);
  };
  take('ext-f-mode', preset.mode);
  take('ext-f-command', preset.command);
  var argsEl = extModalEl('ext-f-args');
  if (argsEl && !argsEl.value.trim()) argsEl.value = extLinesToText(preset.args);
  var allowEl = extModalEl('ext-f-allow');
  if (allowEl && !allowEl.value.trim()) allowEl.value = extLinesToText(preset.allow);
}

function showExtError(msg) {
  var err = extModalEl('modal-ext-err');
  if (!err) return;
  err.textContent = msg || '';
  err.style.display = msg ? 'block' : 'none';
}

/* Collect the form into the authoring layer's shape. On create the name goes
 * with everything else; on update the name comes from the path. Every field is
 * sent, and an emptied whitelist is an empty array — the layer reads that as
 * "clear", which is exactly what the author meant by emptying the box. */
function extCollectForm(includeName) {
  var val = function (id) { var el = extModalEl(id); return el ? el.value : ''; };
  var lines = function (id) { return extTextToLines(val(id)); };
  var out = {
    kind: val('ext-f-kind'),
    target: val('ext-f-target'),
    description: val('ext-f-desc'),
    command: val('ext-f-command'),
    args: lines('ext-f-args'),
    mode: val('ext-f-mode'),
    allow: lines('ext-f-allow'),
    paths: lines('ext-f-paths'),
    notes: val('ext-f-notes')
  };
  var enEl = extModalEl('ext-f-enabled');
  out.enabled = enEl ? !!enEl.checked : true;
  if (includeName) out.name = val('ext-f-name').trim();
  return out;
}

function extSaveTemplate() {
  var name = _extEditing;
  var forCreate = !name;
  var body = extCollectForm(forCreate);
  if (forCreate && !body.name) {
    showExtError(t('ext.modal.nameRequired'));
    return;
  }
  var saveBtn = extModalEl('modal-ext-save');
  if (saveBtn) saveBtn.disabled = true;
  var url = forCreate
    ? apiPath('/api/extensions')
    : apiPath('/api/extensions/') + encodeURIComponent(name);
  fetch(url, {
    method: forCreate ? 'POST' : 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  }).then(function (r) {
    if (saveBtn) saveBtn.disabled = false;
    if (!r.ok) {
      return r.text().then(function (txt) {
        throw new Error(txt || r.status);
      });
    }
    // The store's change subscription makes the reconciler broadcast, and the
    // socket frame repainting the grid is the receipt — no refetch here.
    closeExtensionEditor();
  }).catch(function (err) {
    if (saveBtn) saveBtn.disabled = false;
    showExtError(t('ext.modal.saveFailed') + ' ' + ((err && err.message) || err));
  });
}

function closeExtensionEditor() {
  var modal = extModalEl('modal-ext');
  if (modal) modal.classList.add('hidden');
  _extEditing = null;
  showExtError('');
}

/* Delete is a confirm, because the template is a file but the extension is a
 * connection the reconciler was holding warm — the author is removing both. */
function deleteExtension(name, btn) {
  if (!name) return;
  confirmDialog({
    title: t('ext.delete.title'),
    message: t('ext.delete.confirm', { name: name })
  }).then(function (ok) {
    if (!ok) return;
    if (btn) btn.disabled = true;
    fetch(apiPath('/api/extensions/') + encodeURIComponent(name), { method: 'DELETE' })
      .then(function (r) {
        if (btn) btn.disabled = false;
        if (!r.ok) {
          return r.text().then(function (txt) {
            throw new Error(txt || r.status);
          });
        }
      })
      .catch(function (err) {
        if (btn) btn.disabled = false;
        console.error('extension delete failed: ' + ((err && err.message) || err));
      });
  });
}

(function () {
  var add = document.getElementById('ext-add');
  if (add) add.addEventListener('click', function () { openExtensionEditor(null); });

  var modal = extModalEl('modal-ext');
  if (!modal) return;
  extModalEl('modal-ext-close').addEventListener('click', closeExtensionEditor);
  extModalEl('modal-ext-cancel').addEventListener('click', closeExtensionEditor);
  extModalEl('modal-ext-save').addEventListener('click', extSaveTemplate);
  var kindSel = extModalEl('ext-f-kind');
  if (kindSel) {
    kindSel.addEventListener('change', function () { extSeedFromKind(kindSel.value); });
  }
  modal.addEventListener('click', function (e) {
    if (e.target === modal) closeExtensionEditor();
  });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    if (!modal.classList.contains('hidden')) {
      e.stopImmediatePropagation();
      closeExtensionEditor();
    }
  }, true);
})();
