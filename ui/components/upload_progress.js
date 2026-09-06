(function () {
  'use strict';

  // Byte-level progress for any multipart upload form.
  //
  // A form opts in by naming its own bar:
  //
  //     <form hx-post="…" data-upload-progress="#fw-upload-progress">
  //
  // which is the same idiom as data-gate-input on the submit buttons: the
  // wiring rides the markup it belongs to, so a form cannot be moved, renamed
  // or copied into another panel and quietly lose its progress bar. The
  // alternative — a list of form ids in here — is how the firmware panel came
  // to render a progress bar that nothing ever unhid.
  //
  // File uploads never leave the browser tab, so htmx's own htmx:xhr:progress
  // (fired on the requesting element for both phases) is the real byte-level
  // source of truth. Mirroring loaded/total onto the bar's aria attributes is
  // enough — progress.js's MutationObserver repaints the indicator and label.

  function barFor(el) {
    if (!el || !el.getAttribute) return null;
    var sel = el.getAttribute('data-upload-progress');
    return sel ? document.querySelector(sel) : null;
  }

  document.body.addEventListener('htmx:xhr:progress', function (evt) {
    var bar = barFor(evt.target);
    if (!bar || !evt.detail || !evt.detail.lengthComputable) return;
    // The hidden PROPERTY, matching the server-rendered hidden attribute —
    // preflight's [hidden] { display:none !important } beats a utility class.
    bar.hidden = false;
    bar.setAttribute('aria-valuemax', String(evt.detail.total));
    bar.setAttribute('aria-valuenow', String(evt.detail.loaded));
  });

  // Reset once the request settles so the next upload starts from 0%.
  document.body.addEventListener('htmx:afterRequest', function (evt) {
    var bar = barFor(evt.target);
    if (!bar) return;
    bar.hidden = true;
    bar.setAttribute('aria-valuenow', '0');
  });
})();
