(function () {
  'use strict';

  // Upload/Fetch submit buttons start disabled (see button.Props.Disabled in
  // virtual_media.templ) and only enable once their paired input has a
  // value — htmx re-issues these listeners for free since the elements are
  // swapped back in with the same ids on every render.
  var gates = [
    ['#vm-upload-file', '#vm-upload-submit'],
    ['#vm-fetch-url', '#vm-fetch-submit'],
  ];

  // syncGate reads live DOM state rather than trusting the event that
  // triggered it, so it is correct regardless of HOW the value got there.
  // 'input'/'change' cover typing and the native file picker, but several
  // real paths fill a field without firing either: bfcache/back-forward
  // form restoration (Chrome replays no events on pageshow), browser
  // autofill in some engines, and — notably on Linux — X11 middle-click
  // primary-selection paste, which has a history of landing the text
  // without a trailing input event depending on focus timing. A gate that
  // only reacts to specific events can show a filled field next to a
  // permanently disabled button; recomputing from .value on every plausible
  // trigger makes it correct by construction instead of by event-coverage
  // luck.
  function syncGate(pair) {
    var input = document.querySelector(pair[0]);
    var btn = document.querySelector(pair[1]);
    if (input && btn) btn.disabled = !input.value;
  }

  function syncAllGates() {
    gates.forEach(syncGate);
  }

  function syncGateFor(target) {
    gates.forEach(function (pair) {
      if (target.matches && target.matches(pair[0])) syncGate(pair);
    });
  }

  document.body.addEventListener('input', function (e) {
    syncGateFor(e.target);
  });
  document.body.addEventListener('change', function (e) {
    syncGateFor(e.target);
  });
  // Paste fires before the browser has inserted the text, so read after the
  // event loop turns over rather than off the event itself.
  document.body.addEventListener('paste', function (e) {
    setTimeout(function () { syncGateFor(e.target); }, 0);
  });

  // Covers every path that fills a field without an event: the initial
  // load, an htmx swap that brings the Add Media form back with server-side
  // (or browser-restored) values already present, and returning to the page
  // via back/forward navigation (bfcache), which Chrome does not replay
  // input events for.
  syncAllGates();
  document.body.addEventListener('htmx:afterSwap', syncAllGates);
  window.addEventListener('pageshow', syncAllGates);

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
