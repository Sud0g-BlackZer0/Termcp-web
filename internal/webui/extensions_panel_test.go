package webui

import (
	"strings"
	"testing"
)

// TestExtensionPanelWearsTheBatchChrome pins that the extension column is the
// connections column's other side in the chrome as well as in the data: the same
// batch keys on top (select, invert, open a terminal per ticked extension, export
// the selection, delete it) and the same pair of bottom keys (author one, import
// a set). The classes are the ones workspace.css already dresses, so the two
// columns cannot drift into looking like different controls.
func TestExtensionPanelWearsTheBatchChrome(t *testing.T) {
	index := readAssetLF(t, "index.html")
	for _, want := range []string{
		`id="ext-select-toggle"`,
		`class="nethub-select-toggle" id="ext-select-toggle"`,
		`id="ext-toggle-count"`,
		`id="ext-sel-invert"`,
		`id="ext-open-split"`,
		`id="ext-open-caret"`,
		`id="ext-menu-export"`,
		`id="ext-menu-delete"`,
		`id="ext-add"`,
		`id="ext-import-open"`,
		`class="drawer-add drawer-add-grow" id="ext-add"`,
		`class="drawer-add drawer-import" id="ext-import-open"`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("the extension panel is missing %s", want)
		}
	}
	// The sub-keys (keyboard help, the disabled state) restate nothing: they are
	// the same plate and the same icon set the host toolbar uses.
	if !strings.Contains(index, `class="nethub-batch-icon" id="ext-sel-invert"`) ||
		!strings.Contains(index, `class="nethub-menu-item nethub-menu-danger" id="ext-menu-delete"`) {
		t.Error("the extension batch keys must wear the host toolbar's own classes")
	}
}

// TestExtensionPanelBatchBehaviour pins what the batch keys do, by the calls they
// make: the selection is a set the panel derives its cards from, the export goes
// through the shared TOML download, and the delete is the same comma-batch the
// session and profile routes use — so an extension is deleted exactly like a
// profile is.
func TestExtensionPanelBatchBehaviour(t *testing.T) {
	ext := readAssetLF(t, "static/js/extensions.js")
	for _, want := range []string{
		// The column's own ancestor class is what reveals its keys…
		"body.classList.toggle('ext-selecting', _extSelectMode)",
		// …and the selection lives in a set, never in the DOM.
		"var _extSelIds = new Set()",
		"tile.classList.toggle('batch-selected', _extSelectMode && _extSelIds.has(name))",
		// Export: one shared download helper, one export route.
		"return downloadTOML(path, 'termcp-extensions.toml')",
		"apiPath('/api/extensions/batch')",
		// Delete: the comma batch the other resources already share.
		"deleteResourcesBatch('extensions', names)",
		// Import: the panel's other bottom key.
		"apiPath('/api/extensions/batch'), {\n      method: 'POST'",
		"showModal('modal-ext-import')",
	} {
		if !strings.Contains(ext, want) {
			t.Errorf("extensions.js is missing %q", want)
		}
	}
	// A card opens the status page; in select mode the same click ticks instead.
	if !strings.Contains(ext, "if (_extSelectMode) { toggleExtSelection(name, tile); return; }") ||
		!strings.Contains(ext, "openExtensionDialog(name, 'status')") {
		t.Error("a card click must open the status page unless the column is selecting")
	}
	if !strings.Contains(ext, "openExtensionDialog(null, 'edit')") {
		t.Error("the New key must open the editor with no status page to report on")
	}
}

// TestExtensionDialogHasTwoPages pins the dialog's shape: a status page for the
// card and a configuration page for its gear, reached from those two things and
// not from a tab row between them. The configuration is the TOML file — the only
// editor there is, because a field form would be a second description of the same
// template to keep in step — and creating reuses it with an editable name and a
// seeded document.
func TestExtensionDialogHasTwoPages(t *testing.T) {
	index := readAssetLF(t, "index.html")
	for _, want := range []string{
		`id="ext-page-status"`,
		`id="ext-page-config"`,
		`id="ext-status-word"`,
		`id="ext-status-grid"`,
		`id="ext-status-fails"`,
		`id="ext-status-retry"`,
		`id="ext-status-toggle"`,
		`id="ext-status-delete"`,
		`id="ext-status-close"`,
		`id="ext-f-name"`,
		`id="ext-f-toml"`,
		`id="modal-ext-save"`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("the extension dialog is missing %s", want)
		}
	}
	// What the cut retired: the tabs, the field form, and the key that raised a
	// terminal — the panel reports and configures, an agent drives.
	for _, dead := range []string{
		`id="ext-tab-status"`, `id="ext-tab-edit"`, `id="ext-tabs"`,
		`id="ext-page-edit"`, `id="ext-form-view"`, `id="ext-config-view"`,
		`id="ext-toggle-view"`, `id="ext-status-open"`,
		`id="ext-f-kind"`, `id="ext-f-command"`, `id="ext-f-allow"`, `id="ext-f-enabled"`,
	} {
		if strings.Contains(index, dead) {
			t.Errorf("the dialog still carries %s, which the panel no longer shows", dead)
		}
	}

	ext := readAssetLF(t, "static/js/extensions.js")
	// The gear opens the configuration, the card opens the status page.
	if !strings.Contains(ext, "openExtensionDialog(name, 'config')") ||
		!strings.Contains(ext, "openExtensionDialog(name, 'status')") {
		t.Error("the card and its gear must open the two pages")
	}
	if !strings.Contains(ext, `class="conn-edit-btn"`) {
		t.Error("the configuration must be reached through the gear the host cards also wear")
	}
	// The page reads and writes the file itself: one content type, and the name in
	// the query (a document has no name key — the directory is the name).
	if !strings.Contains(ext, "encodeURIComponent(name) + '?format=toml'") {
		t.Error("the configuration page must read the file the store holds")
	}
	if !strings.Contains(ext, "headers: { 'Content-Type': 'application/toml; charset=utf-8' }") {
		t.Error("the configuration page must save the document itself")
	}
	if !strings.Contains(ext, "query = '?name=' + encodeURIComponent(newName)") {
		t.Error("a create must carry its name, which the file itself has no place for")
	}
	// Creating lands on the configuration page with a seeded, documented document.
	if !strings.Contains(ext, "area.value = extTomlSeed();") {
		t.Error("a create must start from the default template")
	}
	if !strings.Contains(ext, "# A termcp extension. kind + target is enough") {
		t.Error("the default template must explain its own fields")
	}
}

// TestExtensionPanelIsResponsive pins the narrow behaviour the column shares with
// the connections panel — the same drawer, the same trigger, the same shared
// scrim — plus the card shape the extension cards need at that width.
func TestExtensionPanelIsResponsive(t *testing.T) {
	ext := readAssetLF(t, "static/js/extensions.js")
	if !strings.Contains(ext, "return (window.innerWidth || 0) < 800;") {
		t.Error("the extension column must become a drawer below the same breakpoint the host list uses")
	}
	if !strings.Contains(ext, "paintDrawerScrim()") {
		t.Error("the drawer must share the page scrim with the host list")
	}
	css := readAssetLF(t, "static/css/workspace.css")
	for _, want := range []string{
		// The cards fill the column at both widths rather than wrapping at the
		// entry card's own minimum.
		"#ext-grid .conn-tile.entry-card { width: 100%; min-width: 0; max-width: none; }",
		// The keys are revealed by THIS column's class, and the host row's by its
		// own: one selector list, two hosts, no cross-lighting.
		".nethub-selecting .nethub-batch-icon,\n.ext-selecting .nethub-batch-icon { display: inline-flex;",
		".nethub-selecting .nethub-open-split,\n.ext-selecting .nethub-open-split { display: block;",
		".nethub-selecting .conn-tile.node-card.batch-selected,\n.ext-selecting .conn-tile.ext-card.batch-selected {",
		// A ticked extension card reads as armed, exactly as a ticked host does.
		".ext-selecting .conn-tile.ext-card .entry-card-inner { cursor: pointer; }",
		// The dialog's status readout folds its labels on a phone.
		".ext-status-label { flex: 0 0 68px; font-size: .56rem; }",
		"#ext-f-toml { width: 100%; min-height: 240px;",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("workspace.css is missing %q", want)
		}
	}
	// The keys the rework retired: the cards no longer carry their own edit and
	// delete buttons (the status page does), and the New key is the shared
	// bottom-row pair rather than a toolbar button of its own.
	for _, dead := range []string{".ext-edit-btn", ".ext-del-btn", ".ext-add-btn"} {
		if strings.Contains(css, dead) {
			t.Errorf("workspace.css still dresses %s, which no element wears", dead)
		}
	}
	// The cyberpunk theme is a full copy of the panel's sheet, so it must carry
	// the same two-host selectors.
	cyber := readAssetLF(t, "themes/cyberpunk/assets/static/css/workspace.css")
	for _, want := range []string{
		".ext-selecting .nethub-batch-icon {",
		".ext-selecting .nethub-open-split {",
		".ext-selecting .conn-tile.ext-card .entry-card-inner { cursor: pointer; }",
		".ext-status-row {",
	} {
		if !strings.Contains(cyber, want) {
			t.Errorf("the cyberpunk theme is missing %q", want)
		}
	}
}

// TestExtensionToolbarHasOneTick pins the toolbar's iconography at the level it
// broke: the invert key carries both of its icons (hollow and solid) and shows one
// at a time. The rule that hides the inactive one was written against the host
// panel's id, so the extension panel's own key rendered both — two ticks in one
// key. It is scoped by the class both panels share now, which is what this pins.
func TestExtensionToolbarHasOneTick(t *testing.T) {
	for _, sheet := range []string{
		"static/css/workspace.css",
		"themes/cyberpunk/assets/static/css/workspace.css",
	} {
		css := readAssetLF(t, sheet)
		if !strings.Contains(css, ".nethub-batch-icon .ic-solid { display: none; }") {
			t.Errorf("%s: the invert key must show one icon per state, scoped by class", sheet)
		}
		for _, idScoped := range []string{"#nethub-sel-invert", "#nethub-batch-open", "#nethub-open-caret"} {
			if strings.Contains(css, idScoped) {
				t.Errorf("%s: %s scopes shared chrome to one panel's id; the other panel's keys miss the rule", sheet, idScoped)
			}
		}
	}
	// Both panels' keys are addressed by name, and both carry the same glyphs.
	index := readAssetLF(t, "index.html")
	// The open key belongs to the host list alone: dialling a session is what a
	// host card's action means, and an extension's session is already up.
	panel := between(t, index, `<aside class="extpanel" id="extpanel">`, "</aside>")
	if strings.Contains(panel, `class="batch-open-main"`) || strings.Contains(panel, "ext-batch-open") {
		t.Error("the extension toolbar must not carry an open key: it has no session to dial and no terminal to raise")
	}
	if n := strings.Count(index, `class="batch-open-main"`); n != 1 {
		t.Errorf("only the host list carries an open key, found %d", n)
	}
	if n := strings.Count(index, `class="batch-open-caret"`); n != 2 {
		t.Errorf("both panels' caret keys need the shared class, found %d", n)
	}
	host := between(t, index, `<aside class="nethub" id="nethub">`, "</aside>")
	const switchGlyph = `M3.1 4.1l1.1 1.1 2.2-2.4`
	if strings.Contains(panel, switchGlyph) != strings.Contains(host, switchGlyph) {
		t.Error("the two panels' select switches must wear the same glyph")
	}
}

// TestExtensionEmptyStateHasNoKeyOfItsOwn pins that the empty column says what an
// extension is and nothing else: the column already closes with New and Import,
// and a third "create one" in the middle is the same action twice.
func TestExtensionEmptyStateHasNoKeyOfItsOwn(t *testing.T) {
	ext := readAssetLF(t, "static/js/extensions.js")
	empty := between(t, ext, "function renderExtensionEmpty()", "\n}\n")
	if strings.Contains(empty, "<button") || strings.Contains(empty, "ext-empty-add") {
		t.Errorf("the empty state must not carry a key of its own:\n%s", empty)
	}
	if !strings.Contains(empty, "t('ext.empty.text')") {
		t.Error("the empty state still has to say what an extension is")
	}
	// The styles and the string that dressed that key are gone with it.
	css := readAssetLF(t, "static/css/workspace.css")
	if strings.Contains(css, "ext-empty-btn") {
		t.Error("workspace.css still dresses the retired empty-state key")
	}
	cyber := readAssetLF(t, "themes/cyberpunk/assets/static/css/workspace.css")
	if strings.Contains(cyber, "ext-empty-btn") {
		t.Error("the cyberpunk theme still dresses the retired empty-state key")
	}
}
