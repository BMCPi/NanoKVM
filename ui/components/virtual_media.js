(function () {
  'use strict';

  // Extensions DecompressingReader (pkg/utils/decompress.go) sniffs by magic
  // bytes server-side. The client cannot do that mid-upload — the server
  // hasn't seen a byte yet when the request is dispatched — so this reads
  // the chosen filename instead. That is a guess, not a detection: it is
  // only ever shown as the in-flight phase label, never in the completion
  // toast, which reports the server's own sniffed format. A mismatch here
  // (e.g. a mislabeled .gz that is actually plain) is therefore cosmetic.
  var COMPRESSED_EXT = /\.(xz|gz|zst)$/i;

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

  // Reset once the request settles so the next upload starts from 0%.
  document.body.addEventListener('htmx:afterRequest', function (evt) {
    if (!evt.target || evt.target.id !== 'vm-upload-form') return;
    var bar = document.getElementById('vm-upload-progress');
    if (!bar) return;
    bar.hidden = true;
    bar.setAttribute('aria-valuenow', '0');
  });
})();
