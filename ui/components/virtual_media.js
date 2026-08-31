(function () {
  'use strict';

  // Extensions DecompressingReader (pkg/utils/decompress.go) sniffs by magic
  // bytes server-side. The client cannot do that mid-upload — the server
  // hasn't seen a byte yet when the request is dispatched — so this reads
  // the chosen filename instead. That is a guess, not a detection: it is
  // only ever shown as the in-flight phase label, never in the completion
  // toast, which reports the server's own sniffed format. A mismatch here
  // (e.g. a mislabeled .gz that is actually plain) is therefore cosmetic.
  // Kept in step with utils.CompressionExtensions() by
  // TestUploadPhaseLabelKnowsEveryCompressionExtension — a codec the picker
  // offers but this pattern misses reports a plain "Uploading…" for a
  // transfer that spends most of its time decompressing.
  var COMPRESSED_EXT = /\.(xz|gzip|gz|zstd|zst)$/i;

  function uploadPhaseLabel(filename) {
    var ext = COMPRESSED_EXT.exec(filename || '');
    if (!ext) return 'Uploading…';
    return 'Uploading & extracting (' + ext[1].toLowerCase() + ')…';
  }

  // Set once as the request goes out, from the file actually attached —
  // not on every progress tick, since the choice can't change mid-flight.
  document.body.addEventListener('htmx:configRequest', function (evt) {
    if (!evt.target || evt.target.id !== 'vm-upload-form') return;
    var label = document.getElementById('vm-upload-label');
    var file = document.getElementById('vm-upload-file');
    if (!label || !file) return;
    var name = file.files && file.files[0] && file.files[0].name;
    label.textContent = uploadPhaseLabel(name);
  });

  // File uploads never leave the browser tab, so htmx's own
  // htmx:xhr:progress event (fired on the requesting element for both the
  // download and upload phases) is the real byte-level source of truth.
  // Mirroring loaded/total onto the progress bar's aria attributes is
  // enough — progress.js's MutationObserver repaints the indicator/label.
  document.body.addEventListener('htmx:xhr:progress', function (evt) {
    if (!evt.target || evt.target.id !== 'vm-upload-form') return;
    if (!evt.detail || !evt.detail.lengthComputable) return;
    var bar = document.getElementById('vm-upload-progress');
    if (!bar) return;
    // The hidden PROPERTY, matching the server-rendered hidden attribute —
    // see the note beside the Progress in virtual_media.templ.
    bar.hidden = false;
    bar.setAttribute('aria-valuemax', String(evt.detail.total));
    bar.setAttribute('aria-valuenow', String(evt.detail.loaded));
  });

  // ----- Existing tab: the Mount / Delete split button --------------------
  //
  // The chevron menu picks which action the primary button performs. Both
  // buttons are server-rendered and only their visibility changes: htmx
  // captures hx-post when it processes an element, so rewriting the verb on
  // one shared button would keep firing the old request while the label
  // advertised the new one.
  //
  // Nothing persists the choice. An htmx swap re-renders the panel with Mount
  // showing again, which is deliberate — a sticky destructive mode is one
  // stray click away from unlinking the next image too.
  function setMediaAction(mode) {
    var mount = document.getElementById('vm-mount-submit');
    var del = document.getElementById('vm-delete-submit');
    if (!mount || !del) return;
    var deleting = mode === 'delete';
    // The hidden PROPERTY, matching the server-rendered attribute — see the
    // note beside the Delete button in virtual_media.templ.
    mount.hidden = deleting;
    del.hidden = !deleting;
  }

  // dropdownmenu.js portals the menu's content to <body>, so the items are
  // not inside the form by the time they are clicked. Listening on body is
  // what reaches them.
  document.body.addEventListener('click', function (evt) {
    if (!evt.target || !evt.target.closest) return;
    var item = evt.target.closest('[data-vm-action]');
    if (!item) return;
    setMediaAction(item.getAttribute('data-vm-action'));
  });

  // Reset once the request settles so the next upload starts from 0%.
  document.body.addEventListener('htmx:afterRequest', function (evt) {
    if (!evt.target || evt.target.id !== 'vm-upload-form') return;
    var bar = document.getElementById('vm-upload-progress');
    if (!bar) return;
    bar.hidden = true;
    bar.setAttribute('aria-valuenow', '0');
  });
})();
