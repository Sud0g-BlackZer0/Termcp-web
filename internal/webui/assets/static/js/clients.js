/**
 * Connected clients — who is attached to this instance (issue #99).
 *
 * Two kinds of caller, reported side by side and never merged:
 *
 *   MCP    an agent driving sessions. It announced a name during initialize
 *          ("claude-ai 0.1.0"), and it may have claimed a namespace, which is
 *          what tells you *which* agent it is when several share this instance.
 *   Web UI a browser tab holding this page's socket, with the address it came
 *          from and how many sessions it is streaming.
 *
 * They are different transports with different lifetimes, so they get different
 * rows rather than one shape with a kind discriminator and half its fields empty.
 *
 * The data is pushed, not polled: both lists ride the session frame, which the
 * server already sends when a session, a client or a viewer changes. This module
 * therefore only renders what it is handed.
 */

/** Render one row. Built from textContent, never innerHTML: a client name and a
 *  user agent are both attacker-controlled strings. */
function clientRow(kind, title, meta, detail) {
  var row = document.createElement('div');
  row.className = 'clients-row';
  row.setAttribute('role', 'listitem');

  var badge = document.createElement('span');
  badge.className = 'clients-badge clients-badge-' + kind;
  badge.textContent = kind === 'mcp' ? 'MCP' : 'WEB';
  row.appendChild(badge);

  var body = document.createElement('div');
  body.className = 'clients-body';
  var name = document.createElement('div');
  name.className = 'clients-name';
  name.textContent = title;
  body.appendChild(name);

  if (meta) {
    var metaEl = document.createElement('div');
    metaEl.className = 'clients-meta';
    metaEl.textContent = meta;
    body.appendChild(metaEl);
  }
  if (detail) {
    var detailEl = document.createElement('div');
    detailEl.className = 'clients-detail';
    detailEl.textContent = detail;
    body.appendChild(detailEl);
  }
  row.appendChild(body);
  return row;
}

/** A client that never said its name is still a client: the spec makes clientInfo
 *  optional, so the fallback names the fact rather than inventing an identity. */
function mcpClientTitle(c) {
  var name = c && c.name ? String(c.name) : t('clients.unnamed');
  var version = c && c.version ? ' ' + String(c.version) : '';
  return name + version;
}

/** The agent's own label for itself, when it made one. */
function mcpClientDetail(c) {
  var parts = [];
  if (c && c.namespace) parts.push(t('clients.namespace', { ns: String(c.namespace) }));
  if (c && c.connected_at) parts.push(t('clients.since', { time: shortClock(c.connected_at) }));
  return parts.join(' · ');
}

/** A viewer's browser, shortened to something that fits a narrow panel. The
 *  user agent is the only thing distinguishing two tabs from one machine. */
function shortUserAgent(ua) {
  var text = String(ua || '');
  // Chrome/Edge/Safari's own token is the useful part; the compatibility noise
  // in front of it is the same for every browser.
  var m = /(Firefox|Edg|Chrome|Safari|curl|wget)\/[\d.]+/.exec(text);
  if (m) return m[0];
  return text.length > 40 ? text.slice(0, 40) + '…' : text;
}

/** RFC 3339 in, "HH:MM" out, in the reader's own zone — a connection timestamp is
 *  read as "when", and the date is implied by it being a live connection. */
function shortClock(rfc3339) {
  var d = new Date(rfc3339);
  if (isNaN(d.getTime())) return String(rfc3339);
  return d.toLocaleTimeString(_termcpLang === 'en' ? 'en' : _termcpLang, { hour: '2-digit', minute: '2-digit' });
}

/** Paint the panel from the session frame's client lists.
 *
 *  Called for every frame, so it must be cheap and must not touch anything when
 *  the panel is closed: the frame arrives many times per second while a command
 *  streams, and rebuilding a hidden DOM list each time is pure waste. */
var _clientsSignature = '';
function renderClients(mcpClients, viewers) {
  var count = document.getElementById('clients-count');
  var body = document.getElementById('clients-body');
  if (!body || !count) return;
  mcpClients = Array.isArray(mcpClients) ? mcpClients : [];
  viewers = Array.isArray(viewers) ? viewers : [];

  /* The count says how many clients there are BESIDES this page. Every open tab
     is itself a viewer, so counting every row meant a lone page always showed
     "1" — and the empty state could never appear at all, which is exactly how
     this was reported. The current tab is still listed in the panel (that is how
     a person confirms the list works), marked as "this tab" and left out of the
     number. */
  var selfCount = 0;
  viewers.forEach(function (v) { if (v && v.self) selfCount++; });
  var total = mcpClients.length + viewers.length - selfCount;
  count.textContent = String(total);
  count.hidden = total === 0;

  var menu = document.getElementById('clients-menu');
  if (menu && !menu.open) return;

  /* Skip the rebuild when nothing changed. A frame arrives per output batch; with
     a terminal streaming that is thousands of identical repaints of a list that
     changes once a minute. The signature is the rendered content, not the input:
     two frames that would draw the same panel are the same frame here. */
  var signature = JSON.stringify([mcpClients, viewers]);
  if (signature === _clientsSignature) return;
  _clientsSignature = signature;

  body.textContent = '';
  mcpClients.forEach(function (c) {
    body.appendChild(clientRow('mcp', mcpClientTitle(c), mcpClientDetail(c), c && c.session_id ? String(c.session_id) : ''));
  });
  viewers.forEach(function (v) {
    var meta = shortUserAgent(v && v.user_agent);
    if (v && typeof v.watching === 'number' && v.watching > 0) {
      meta += (meta ? ' · ' : '') + t('clients.watching', { count: v.watching });
    }
    // This tab says so, so the row below the count is explainable rather than a
    // mystery extra client.
    var title = v && v.addr ? String(v.addr) : '';
    body.appendChild(clientRow('web', title, meta, v && v.self ? t('clients.thisTab') : ''));
  });
  /* The empty state is about the *other* clients: with only this tab open the
     list has one row (yourself), so hiding the hint on `body.childElementCount`
     would leave a panel that looks populated while the badge says 0. */
  var others = mcpClients.length + viewers.length - selfCount;
  var empty = document.getElementById('clients-empty');
  if (empty) empty.hidden = others > 0;
}
